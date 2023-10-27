package playlist

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/grafana/grafana/pkg/services/playlist"
	"github.com/grafana/grafana/pkg/tests/apis"
)

func TestPlaylist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	helper := apis.NewK8sTestHelper(t)
	gvr := schema.GroupVersionResource{
		Group:    "playlist.grafana.app",
		Version:  "v0alpha1",
		Resource: "playlists",
	}

	t.Run("Check direct List permissions from different org users", func(t *testing.T) {
		// Check view permissions
		rsp := helper.List(helper.Org1.Viewer, "default", gvr)
		require.Equal(t, 200, rsp.Response.StatusCode)
		require.NotNil(t, rsp.Result)
		require.Empty(t, rsp.Result.Items)
		require.Nil(t, rsp.Status)

		// Check view permissions
		rsp = helper.List(helper.Org2.Viewer, "default", gvr)
		require.Equal(t, 403, rsp.Response.StatusCode) // Org2 can not see default namespace
		require.Nil(t, rsp.Result)
		require.Equal(t, metav1.StatusReasonForbidden, rsp.Status.Reason)

		// Check view permissions
		rsp = helper.List(helper.Org2.Viewer, "org-22", gvr)
		require.Equal(t, 403, rsp.Response.StatusCode) // Unknown/not a member
		require.Nil(t, rsp.Result)
		require.Equal(t, metav1.StatusReasonForbidden, rsp.Status.Reason)
	})

	t.Run("Check k8s client-go List from different org users", func(t *testing.T) {
		// Check Org1 Viewer
		client := helper.GetResourceClient(apis.ResourceClientArgs{
			User:      helper.Org1.Viewer,
			Namespace: "", // << fills in the value org1 is allowed to see!
			GVR:       gvr,
		})
		rsp, err := client.Resource.List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Empty(t, rsp.Items)

		// Check org2 viewer can not see org1 (default namespace)
		client = helper.GetResourceClient(apis.ResourceClientArgs{
			User:      helper.Org2.Viewer,
			Namespace: "default", // actually org1
			GVR:       gvr,
		})
		rsp, err = client.Resource.List(context.Background(), metav1.ListOptions{})
		statusError := helper.AsStatusError(err)
		require.Nil(t, rsp)
		require.Equal(t, metav1.StatusReasonForbidden, statusError.Status().Reason)

		// Check invalid namespace
		client = helper.GetResourceClient(apis.ResourceClientArgs{
			User:      helper.Org2.Viewer,
			Namespace: "org-22", // org 22 does not exist
			GVR:       gvr,
		})
		rsp, err = client.Resource.List(context.Background(), metav1.ListOptions{})
		statusError = helper.AsStatusError(err)
		require.Nil(t, rsp)
		require.Equal(t, metav1.StatusReasonForbidden, statusError.Status().Reason)
	})

	t.Run("Check playlist CRUD in legacy API appears in k8s apis", func(t *testing.T) {
		client := helper.GetResourceClient(apis.ResourceClientArgs{
			User: helper.Org1.Editor,
			GVR:  gvr,
		})

		// This includes the raw dashboard values that are currently sent (but should not be and are ignored)
		legacyPayload := `{
			"name": "Test",
			"interval": "20s",
			"items": [
			  {
				"type": "dashboard_by_uid",
				"value": "xCmMwXdVz",
				"dashboards": [
				  {
					"name": "The dashboard",
					"kind": "dashboard",
					"uid": "xCmMwXdVz",
					"url": "/d/xCmMwXdVz/barchart-label-rotation-and-skipping",
					"tags": ["barchart", "gdev", "graph-ng", "panel-tests"],
					"location": "d1de6240-fd2e-4e13-99b6-f9d0c6b0550d"
				  }
				]
			  },
			  {
				"type": "dashboard_by_tag",
				"value": "graph-ng",
				"dashboards": [ "..." ]
			  }
			],
			"uid": ""
		  }`
		legacyCreate := apis.DoRequest(helper, apis.RequestParams{
			User:   client.Args.User,
			Method: http.MethodPost,
			Path:   "/api/playlists",
			Body:   []byte(legacyPayload),
		}, &playlist.Playlist{})
		require.NotNil(t, legacyCreate.Result)
		uid := legacyCreate.Result.UID
		require.NotEmpty(t, uid)

		expectedResult := `{
			"apiVersion": "playlist.grafana.app/v0alpha1",
			"kind": "Playlist",
			"metadata": {
			  "creationTimestamp": "${creationTimestamp}",
			  "name": "` + uid + `",
			  "namespace": "default",
			  "resourceVersion": "${resourceVersion}",
			  "uid": "${uid}"
			},
			"spec": {
			  "title": "Test",
			  "interval": "20s",
			  "items": [
				{
				  "type": "dashboard_by_uid",
				  "value": "xCmMwXdVz"
				},
				{
				  "type": "dashboard_by_tag",
				  "value": "graph-ng"
				}
			  ]
			}
		  }`

		// List includes the expected result
		k8sList, err := client.Resource.List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Equal(t, 1, len(k8sList.Items))
		require.JSONEq(t, expectedResult, client.SanitizeJSON(&k8sList.Items[0]))

		// Get should return the same result
		found, err := client.Resource.Get(context.Background(), uid, metav1.GetOptions{})
		require.NoError(t, err)
		require.JSONEq(t, expectedResult, client.SanitizeJSON(found))

		// Now modify the interval
		updatedInterval := `"interval": "10m"`
		legacyPayload = strings.Replace(legacyPayload, `"interval": "20s"`, updatedInterval, 1)
		expectedResult = strings.Replace(expectedResult, `"interval": "20s"`, updatedInterval, 1)
		dtoResponse := apis.DoRequest(helper, apis.RequestParams{
			User:   client.Args.User,
			Method: http.MethodPut,
			Path:   "/api/playlists/" + uid,
			Body:   []byte(legacyPayload),
		}, &playlist.PlaylistDTO{})
		require.Equal(t, uid, dtoResponse.Result.Uid)
		require.Equal(t, "10m", dtoResponse.Result.Interval)

		// Make sure the changed interval is now returned from k8s
		found, err = client.Resource.Get(context.Background(), uid, metav1.GetOptions{})
		require.NoError(t, err)
		require.JSONEq(t, expectedResult, client.SanitizeJSON(found))

		// Delete does not return anything
		_ = apis.DoRequest(helper, apis.RequestParams{
			User:   client.Args.User,
			Method: http.MethodDelete,
			Path:   "/api/playlists/" + uid,
			Body:   []byte(legacyPayload),
		}, &playlist.PlaylistDTO{}) // response is empty

		found, err = client.Resource.Get(context.Background(), uid, metav1.GetOptions{})
		statusError := helper.AsStatusError(err)
		require.Nil(t, found)
		require.Equal(t, metav1.StatusReasonNotFound, statusError.Status().Reason)
	})

	t.Run("Do CRUD via k8s (and check that legacy api still works)", func(t *testing.T) {
		client := helper.GetResourceClient(apis.ResourceClientArgs{
			User: helper.Org1.Editor,
			GVR:  gvr,
		})

		// Create the playlist "abcdefgh"
		first, err := client.Resource.Create(context.Background(),
			helper.LoadYAMLOrJSONFile("testdata/playlist-with-uid.yaml"),
			metav1.CreateOptions{},
		)
		require.NoError(t, err)
		require.Equal(t, "abcdefgh", first.GetName())
		uids := []string{first.GetName()}

		// Create (with name generation) two playlists
		for i := 0; i < 2; i++ {
			out, err := client.Resource.Create(context.Background(),
				helper.LoadYAMLOrJSONFile("testdata/playlist-generate.yaml"),
				metav1.CreateOptions{},
			)
			require.NoError(t, err)
			uids = append(uids, out.GetName())
		}
		slices.Sort(uids) // make list compare stable

		// Check that everything is returned from the List command
		list, err := client.Resource.List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Equal(t, uids, SortSlice(Map(list.Items, func(item unstructured.Unstructured) string {
			return item.GetName()
		})))

		// The legacy endpoint has the same results
		searchResponse := apis.DoRequest(helper, apis.RequestParams{
			User:   client.Args.User,
			Method: http.MethodGet,
			Path:   "/api/playlists",
		}, &playlist.Playlists{})
		require.NotNil(t, searchResponse.Result)
		require.Equal(t, uids, SortSlice(Map(*searchResponse.Result, func(item *playlist.Playlist) string {
			return item.UID
		})))

		// Get works for both apis
		for _, uid := range uids {
			found, err := client.Resource.Get(context.Background(), uid, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, uid, found.GetName())

			dto := apis.DoRequest(helper, apis.RequestParams{
				User:   client.Args.User,
				Method: http.MethodGet,
				Path:   "/api/playlists/" + uid,
			}, &playlist.PlaylistDTO{})
			require.NotNil(t, dto.Result)
			require.Equal(t, uid, dto.Result.Uid)

			// Check same values
			spec, ok := found.Object["spec"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, dto.Result.Name, spec["title"]) // Name <> Title
			require.Equal(t, dto.Result.Interval, spec["interval"])
		}

		// PUT :: Update the title (full payload)
		updated, err := client.Resource.Update(context.Background(),
			helper.LoadYAMLOrJSON(`
apiVersion: playlist.grafana.app/v0alpha1
kind: Playlist
metadata:
  name: abcdefgh #
spec:
  title: This one is updated and only has one item
  interval: 20s
  items:
  - type: dashboard_by_tag
    value: panel-tests
`),
			metav1.UpdateOptions{},
		)
		require.NoError(t, err)
		require.Equal(t, first.GetName(), updated.GetName())
		require.Equal(t, first.GetUID(), updated.GetUID())
		require.Less(t, first.GetResourceVersion(), updated.GetResourceVersion())
		spec, ok := updated.Object["spec"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "This one is updated and only has one item", spec["title"])
		require.Equal(t, "20s", spec["interval"])

		// PATCH :: apply only some fields
		if false { // DOES NOT WORK YET?????
			updated, err = client.Resource.Apply(context.Background(), "abcdefgh",
				helper.LoadYAMLOrJSON(`
apiVersion: playlist.grafana.app/v0alpha1
kind: Playlist
spec:
  title: The patched title!
`),
				metav1.ApplyOptions{
					Force:        true,
					FieldManager: "testing",
				},
			)
			require.NoError(t, err)
			require.Equal(t, first.GetName(), updated.GetName())
			require.Equal(t, first.GetUID(), updated.GetUID())
			require.Less(t, first.GetResourceVersion(), updated.GetResourceVersion())
			spec, ok = updated.Object["spec"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "The patched title!", spec["title"])
			require.Equal(t, "20s", spec["interval"])
		}

		// Now delete all playlist (three)
		for _, uid := range uids {
			err := client.Resource.Delete(context.Background(), uid, metav1.DeleteOptions{})
			require.NoError(t, err)

			// Second call is not found!
			err = client.Resource.Delete(context.Background(), uid, metav1.DeleteOptions{})
			statusError := helper.AsStatusError(err)
			require.Equal(t, metav1.StatusReasonNotFound, statusError.Status().Reason)

			// Not found from k8s getter
			_, err = client.Resource.Get(context.Background(), uid, metav1.GetOptions{})
			statusError = helper.AsStatusError(err)
			require.Equal(t, metav1.StatusReasonNotFound, statusError.Status().Reason)
		}

		// Check that they are all gone
		list, err = client.Resource.List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Empty(t, list.Items)
	})
}

// typescript style map function
func Map[A any, B any](input []A, m func(A) B) []B {
	output := make([]B, len(input))
	for i, element := range input {
		output[i] = m(element)
	}
	return output
}

func SortSlice[A cmp.Ordered](input []A) []A {
	slices.Sort(input)
	return input
}
