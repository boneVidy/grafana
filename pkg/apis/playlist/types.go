package playlist

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Playlist struct {
	metav1.TypeMeta
	metav1.ObjectMeta

	Spec Spec
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type PlaylistList struct {
	metav1.TypeMeta
	metav1.ListMeta

	Items []Playlist
}

// Spec defines model for Spec.
type Spec struct {
	// Name of the playlist.
	Title string

	// Interval sets the time between switching views in a playlist.
	Interval string

	// The ordered list of items that the playlist will iterate over.
	Items []Item
}

// Defines values for ItemType.
const (
	ItemTypeDashboardByTag ItemType = "dashboard_by_tag"
	ItemTypeDashboardByUid ItemType = "dashboard_by_uid"

	// deprecated -- should use UID
	ItemTypeDashboardById ItemType = "dashboard_by_id"
)

// Item defines model for Item.
type Item struct {
	// Type of the item.
	Type ItemType

	// Value depends on type and describes the playlist item.
	//
	//  - dashboard_by_id: The value is an internal numerical identifier set by Grafana. This
	//  is not portable as the numerical identifier is non-deterministic between different instances.
	//  Will be replaced by dashboard_by_uid in the future. (deprecated)
	//  - dashboard_by_tag: The value is a tag which is set on any number of dashboards. All
	//  dashboards behind the tag will be added to the playlist.
	//  - dashboard_by_uid: The value is the dashboard UID
	Value string
}

// Type of the item.
type ItemType string
