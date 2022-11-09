package state

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/services/annotations"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/ngalert/eval"
	"github.com/grafana/grafana/pkg/services/ngalert/image"
	"github.com/grafana/grafana/pkg/services/ngalert/metrics"
	ngModels "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
)

var ResendDelay = 30 * time.Second

// AlertInstanceManager defines the interface for querying the current alert instances.
type AlertInstanceManager interface {
	GetAll(orgID int64) []*State
	GetStatesForRuleUID(orgID int64, alertRuleUID string) []*State
}

type Manager struct {
	log     log.Logger
	metrics *metrics.State

	clock       clock.Clock
	cache       *cache
	quit        chan struct{}
	ResendDelay time.Duration

	ruleStore        store.RuleStore
	instanceStore    store.InstanceStore
	dashboardService dashboards.DashboardService
	imageService     image.ImageService
	AnnotationsRepo  annotations.Repository
}

func NewManager(logger log.Logger, metrics *metrics.State, externalURL *url.URL,
	ruleStore store.RuleStore, instanceStore store.InstanceStore,
	dashboardService dashboards.DashboardService, imageService image.ImageService, clock clock.Clock, annotationsRepo annotations.Repository) *Manager {
	manager := &Manager{
		cache:            newCache(logger, metrics, externalURL),
		quit:             make(chan struct{}),
		ResendDelay:      ResendDelay, // TODO: make this configurable
		log:              logger,
		metrics:          metrics,
		ruleStore:        ruleStore,
		instanceStore:    instanceStore,
		dashboardService: dashboardService,
		imageService:     imageService,
		clock:            clock,
		AnnotationsRepo:  annotationsRepo,
	}
	go manager.recordMetrics()
	return manager
}

func (st *Manager) Close() {
	st.quit <- struct{}{}
}

func (st *Manager) Warm(ctx context.Context) {
	st.log.Info("warming cache for startup")
	st.ResetAllStates()

	orgIds, err := st.instanceStore.FetchOrgIds(ctx)
	if err != nil {
		st.log.Error("unable to fetch orgIds", "msg", err.Error())
	}

	var states []*State
	for _, orgId := range orgIds {
		// Get Rules
		ruleCmd := ngModels.ListAlertRulesQuery{
			OrgID: orgId,
		}
		if err := st.ruleStore.ListAlertRules(ctx, &ruleCmd); err != nil {
			st.log.Error("unable to fetch previous state", "msg", err.Error())
		}

		ruleByUID := make(map[string]*ngModels.AlertRule, len(ruleCmd.Result))
		for _, rule := range ruleCmd.Result {
			ruleByUID[rule.UID] = rule
		}

		// Get Instances
		cmd := ngModels.ListAlertInstancesQuery{
			RuleOrgID: orgId,
		}
		if err := st.instanceStore.ListAlertInstances(ctx, &cmd); err != nil {
			st.log.Error("unable to fetch previous state", "msg", err.Error())
		}

		for _, entry := range cmd.Result {
			ruleForEntry, ok := ruleByUID[entry.RuleUID]
			if !ok {
				st.log.Error("rule not found for instance, ignoring", "rule", entry.RuleUID)
				continue
			}

			lbs := map[string]string(entry.Labels)
			cacheID, err := entry.Labels.StringKey()
			if err != nil {
				st.log.Error("error getting cacheID for entry", "msg", err.Error())
			}
			stateForEntry := &State{
				AlertRuleUID:         entry.RuleUID,
				OrgID:                entry.RuleOrgID,
				CacheID:              cacheID,
				Labels:               lbs,
				State:                translateInstanceState(entry.CurrentState),
				StateReason:          entry.CurrentReason,
				LastEvaluationString: "",
				StartsAt:             entry.CurrentStateSince,
				EndsAt:               entry.CurrentStateEnd,
				LastEvaluationTime:   entry.LastEvalTime,
				Annotations:          ruleForEntry.Annotations,
			}
			states = append(states, stateForEntry)
		}
	}

	for _, s := range states {
		st.set(s)
	}
}

func (st *Manager) getOrCreate(ctx context.Context, alertRule *ngModels.AlertRule, result eval.Result, extraLabels data.Labels) *State {
	return st.cache.getOrCreate(ctx, alertRule, result, extraLabels)
}

func (st *Manager) set(entry *State) {
	st.cache.set(entry)
}

func (st *Manager) Get(orgID int64, alertRuleUID, stateId string) (*State, error) {
	return st.cache.get(orgID, alertRuleUID, stateId)
}

// ResetAllStates is used to ensure a clean cache on startup.
func (st *Manager) ResetAllStates() {
	st.cache.reset()
}

// ResetStateByRuleUID deletes all entries in the state manager that match the given rule UID.
func (st *Manager) ResetStateByRuleUID(ctx context.Context, ruleKey ngModels.AlertRuleKey) []*State {
	logger := st.log.New(ruleKey.LogContext()...)
	logger.Debug("resetting state of the rule")
	states := st.cache.removeByRuleUID(ruleKey.OrgID, ruleKey.UID)
	if len(states) > 0 {
		err := st.instanceStore.DeleteAlertInstancesByRule(ctx, ruleKey)
		if err != nil {
			logger.Error("failed to delete states that belong to a rule from database", ruleKey.LogContext()...)
		}
	}
	logger.Info("rules state was reset", "deleted_states", len(states))
	return states
}

// ProcessEvalResults updates the current states that belong to a rule with the evaluation results.
// if extraLabels is not empty, those labels will be added to every state. The extraLabels take precedence over rule labels and result labels
func (st *Manager) ProcessEvalResults(ctx context.Context, evaluatedAt time.Time, alertRule *ngModels.AlertRule, results eval.Results, extraLabels data.Labels) []*State {
	logger := st.log.New(alertRule.GetKey().LogContext()...)
	logger.Debug("state manager processing evaluation results", "resultCount", len(results))
	var states []*State
	processedResults := make(map[string]*State, len(results))
	for _, result := range results {
		s := st.setNextState(ctx, alertRule, result, extraLabels)
		states = append(states, s)
		processedResults[s.CacheID] = s
	}
	resolvedStates := st.staleResultsHandler(ctx, alertRule, processedResults, evaluatedAt)
	if len(states) > 0 {
		logger.Debug("saving new states to the database", "count", len(states))
		for _, state := range states {
			if err := st.saveState(ctx, state); err != nil {
				logger.Error("failed to save alert state", "labels", state.Labels.String(), "state", state.State.String(), "err", err.Error())
			}
		}
	}
	return append(states, resolvedStates...)
}

// Set the current state based on evaluation results
func (st *Manager) setNextState(ctx context.Context, alertRule *ngModels.AlertRule, result eval.Result, extraLabels data.Labels) *State {
	currentState := st.getOrCreate(ctx, alertRule, result, extraLabels)

	currentState.LastEvaluationTime = result.EvaluatedAt
	currentState.EvaluationDuration = result.EvaluationDuration
	currentState.Results = append(currentState.Results, Evaluation{
		EvaluationTime:  result.EvaluatedAt,
		EvaluationState: result.State,
		Values:          NewEvaluationValues(result.Values),
		Condition:       alertRule.Condition,
	})
	currentState.LastEvaluationString = result.EvaluationString
	currentState.TrimResults(alertRule)
	oldState := currentState.State
	oldReason := currentState.StateReason

	st.log.Debug("setting alert state", "uid", alertRule.UID)
	switch result.State {
	case eval.Normal:
		currentState.resultNormal(alertRule, result)
	case eval.Alerting:
		currentState.resultAlerting(alertRule, result)
	case eval.Error:
		currentState.resultError(alertRule, result)
	case eval.NoData:
		currentState.resultNoData(alertRule, result)
	case eval.Pending: // we do not emit results with this state
	}

	// Set reason iff: result is different than state, reason is not Alerting or Normal
	currentState.StateReason = ""

	if currentState.State != result.State &&
		result.State != eval.Normal &&
		result.State != eval.Alerting {
		currentState.StateReason = result.State.String()
	}

	// Set Resolved property so the scheduler knows to send a postable alert
	// to Alertmanager.
	currentState.Resolved = oldState == eval.Alerting && currentState.State == eval.Normal

	if shouldTakeImage(currentState.State, oldState, currentState.Image, currentState.Resolved) {
		image, err := takeImage(ctx, st.imageService, alertRule)
		if err != nil {
			st.log.Warn("Failed to take an image",
				"dashboard", alertRule.DashboardUID,
				"panel", alertRule.PanelID,
				"error", err)
		} else if image != nil {
			currentState.Image = image
		}
	}

	st.set(currentState)

	shouldUpdateAnnotation := oldState != currentState.State || oldReason != currentState.StateReason
	if shouldUpdateAnnotation {
		go st.annotateState(ctx, alertRule, currentState.Labels, result.EvaluatedAt, InstanceStateAndReason{State: currentState.State, Reason: currentState.StateReason}, InstanceStateAndReason{State: oldState, Reason: oldReason})
	}
	return currentState
}

func (st *Manager) GetAll(orgID int64) []*State {
	return st.cache.getAll(orgID)
}

func (st *Manager) GetStatesForRuleUID(orgID int64, alertRuleUID string) []*State {
	return st.cache.getStatesForRuleUID(orgID, alertRuleUID)
}

func (st *Manager) recordMetrics() {
	// TODO: parameterize?
	// Setting to a reasonable default scrape interval for Prometheus.
	dur := time.Duration(15) * time.Second
	ticker := st.clock.Ticker(dur)
	for {
		select {
		case <-ticker.C:
			st.log.Debug("recording state cache metrics", "now", st.clock.Now())
			st.cache.recordMetrics()
		case <-st.quit:
			st.log.Debug("stopping state cache metrics recording", "now", st.clock.Now())
			ticker.Stop()
			return
		}
	}
}

func (st *Manager) Put(states []*State) {
	for _, s := range states {
		st.set(s)
	}
}

func (st *Manager) saveState(ctx context.Context, s *State) error {
	cmd := ngModels.SaveAlertInstanceCommand{
		RuleOrgID:         s.OrgID,
		RuleUID:           s.AlertRuleUID,
		Labels:            ngModels.InstanceLabels(s.Labels),
		State:             ngModels.InstanceStateType(s.State.String()),
		StateReason:       s.StateReason,
		LastEvalTime:      s.LastEvaluationTime,
		CurrentStateSince: s.StartsAt,
		CurrentStateEnd:   s.EndsAt,
	}
	return st.instanceStore.SaveAlertInstance(ctx, &cmd)
}

// TODO: why wouldn't you allow other types like NoData or Error?
func translateInstanceState(state ngModels.InstanceStateType) eval.State {
	switch {
	case state == ngModels.InstanceStateFiring:
		return eval.Alerting
	case state == ngModels.InstanceStateNormal:
		return eval.Normal
	default:
		return eval.Error
	}
}

// This struct provides grouping of state with reason, and string formatting.
type InstanceStateAndReason struct {
	State  eval.State
	Reason string
}

func (i InstanceStateAndReason) String() string {
	s := fmt.Sprintf("%v", i.State)
	if len(i.Reason) > 0 {
		s += fmt.Sprintf(" (%v)", i.Reason)
	}
	return s
}

func (st *Manager) annotateState(ctx context.Context, alertRule *ngModels.AlertRule, labels data.Labels, evaluatedAt time.Time, currentData, previousData InstanceStateAndReason) {
	st.log.Debug("alert state changed creating annotation", "alertRuleUID", alertRule.UID, "newState", currentData.String(), "oldState", previousData.String())

	labels = removePrivateLabels(labels)
	annotationText := fmt.Sprintf("%s {%s} - %s", alertRule.Title, labels.String(), currentData.String())

	item := &annotations.Item{
		AlertId:   alertRule.ID,
		OrgId:     alertRule.OrgID,
		PrevState: previousData.String(),
		NewState:  currentData.String(),
		Text:      annotationText,
		Epoch:     evaluatedAt.UnixNano() / int64(time.Millisecond),
	}

	dashUid, ok := alertRule.Annotations[ngModels.DashboardUIDAnnotation]
	if ok {
		panelUid := alertRule.Annotations[ngModels.PanelIDAnnotation]

		panelId, err := strconv.ParseInt(panelUid, 10, 64)
		if err != nil {
			st.log.Error("error parsing panelUID for alert annotation", "panelUID", panelUid, "alertRuleUID", alertRule.UID, "err", err.Error())
			return
		}

		query := &models.GetDashboardQuery{
			Uid:   dashUid,
			OrgId: alertRule.OrgID,
		}

		err = st.dashboardService.GetDashboard(ctx, query)
		if err != nil {
			st.log.Error("error getting dashboard for alert annotation", "dashboardUID", dashUid, "alertRuleUID", alertRule.UID, "err", err.Error())
			return
		}

		item.PanelId = panelId
		item.DashboardId = query.Result.Id
	}

	if err := st.AnnotationsRepo.Save(ctx, item); err != nil {
		st.log.Error("error saving alert annotation", "alertRuleUID", alertRule.UID, "err", err.Error())
		return
	}
}

func (st *Manager) staleResultsHandler(ctx context.Context, r *ngModels.AlertRule, states map[string]*State, evaluatedAt time.Time) []*State {
	var (
		// resolvedImage contains the image for all stale states that are resolved. The resolved image is shared between
		// all resolved states as the alert rule is the same. TODO: We will need to change this when we support images
		// without screenshots as each state will have a different image
		resolvedImage *ngModels.Image

		// resolvedStates contains the stale states that were resolved
		resolvedStates []*State

		// knownStates contains the current set of states in the state cache
		knownStates []*State
	)

	knownStates = st.GetStatesForRuleUID(r.OrgID, r.UID)
	for _, s := range knownStates {
		_, ok := states[s.CacheID]
		if !ok && isItStale(evaluatedAt, s.LastEvaluationTime, r.IntervalSeconds) {
			st.log.Debug("removing stale state entry", "orgID", s.OrgID, "alertRuleUID", s.AlertRuleUID, "cacheID", s.CacheID)
			st.cache.deleteEntry(s.OrgID, s.AlertRuleUID, s.CacheID)
			ilbs := ngModels.InstanceLabels(s.Labels)
			_, labelsHash, err := ilbs.StringAndHash()
			if err != nil {
				st.log.Error("unable to get labelsHash", "err", err.Error(), "orgID", s.OrgID, "alertRuleUID", s.AlertRuleUID)
			}

			if err = st.instanceStore.DeleteAlertInstance(ctx, s.OrgID, s.AlertRuleUID, labelsHash); err != nil {
				st.log.Error("unable to delete stale instance from database", "err", err.Error(), "orgID", s.OrgID, "alertRuleUID", s.AlertRuleUID, "cacheID", s.CacheID)
			}

			if s.State == eval.Alerting {
				previousState := InstanceStateAndReason{State: s.State, Reason: s.StateReason}
				s.Resolve(ngModels.StateReasonMissingSeries, evaluatedAt)
				st.annotateState(ctx, r, s.Labels, evaluatedAt,
					InstanceStateAndReason{State: eval.Normal, Reason: s.StateReason},
					previousState,
				)

				// If there is no resolved image for this rule then take one
				if resolvedImage == nil {
					image, err := takeImage(ctx, st.imageService, r)
					if err != nil {
						st.log.Warn("Failed to take an image",
							"dashboard", r.DashboardUID,
							"panel", r.PanelID,
							"error", err)
					} else if image != nil {
						resolvedImage = image
					}
				}
				s.Image = resolvedImage
				resolvedStates = append(resolvedStates, s)
			}
		}
	}
	return resolvedStates
}

func isItStale(evaluatedAt time.Time, lastEval time.Time, intervalSeconds int64) bool {
	return !lastEval.Add(2 * time.Duration(intervalSeconds) * time.Second).After(evaluatedAt)
}

func removePrivateLabels(labels data.Labels) data.Labels {
	result := make(data.Labels)
	for k, v := range labels {
		if !strings.HasPrefix(k, "__") && !strings.HasSuffix(k, "__") {
			result[k] = v
		}
	}
	return result
}
