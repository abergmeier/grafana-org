package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// UserSpec defines the desired state of User
type UserSpec struct {
	// Important: Run "make" to regenerate code after modifying this file

	Url   string        `json:"url"`
	Admin *GrafanaAdmin `json:"admin,omitempty"`
	Users []User        `json:"users,omitempty"`
}

type User struct {
	Name       string   `json:"name,omitempty"`
	Login      string   `json:"login"`
	Email      string   `json:"email,omitempty"`
	AuthLabels []string `json:"authLabels,omitempty"`
}

// UserStatus defines the observed state of User
type UserStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Users is the Schema for the users API
type Users struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UserSpec   `json:"spec,omitempty"`
	Status UserStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// UsersList contains a list of User
type UsersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Users `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Users{}, &UsersList{})
}
