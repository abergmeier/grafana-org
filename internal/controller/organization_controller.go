/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	grafanav1 "github.com/abergmeier/grafana-org/api/v1"
	gapi "github.com/grafana/grafana-api-golang-client"
)

const (
	orgOwnerKey = ".metadata.controller"
)

// OrganizationReconciler reconciles a Organization object
type OrganizationReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	BaseUrl string
}

type email string
type userConfig struct {
	role string
}
type missingUserConfig struct {
	userConfig
	email email
}

type changeUserConfig struct {
	userConfig
	current gapi.OrgUser
}

//+kubebuilder:rbac:groups=grafana.abergmeier.github.com,resources=organizations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=grafana.abergmeier.github.com,resources=organizations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=grafana.abergmeier.github.com,resources=organizations/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *OrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	client, err := gapi.New(r.BaseUrl, gapi.Config{})
	if err != nil {
		panic(err)
	}

	expected, err := r.findExpected(ctx, req)
	if err != nil {
		panic(err)
	}

	currentOrgUsers, err := client.OrgUsersCurrent()
	if err != nil {
		panic(err)
	}

	currentOrgUserMap := map[email]gapi.OrgUser{}
	obsoleteOrgUsers := map[email]gapi.OrgUser{}

	for _, ou := range currentOrgUsers {
		currentOrgUserMap[email(ou.Email)] = ou
		obsoleteOrgUsers[email(ou.Email)] = ou
	}

	missingOrgUsers := []missingUserConfig{}
	changeOrgUsers := []changeUserConfig{}

	for email, uc := range expected {
		alreadyPresentConfig, ok := currentOrgUserMap[email]
		if !ok {
			missingOrgUsers = append(missingOrgUsers, missingUserConfig{
				email:      email,
				userConfig: uc,
			})
			continue
		}

		if uc.role != alreadyPresentConfig.Role {
			changeOrgUsers = append(changeOrgUsers, changeUserConfig{
				userConfig: uc,
			})
		}
	}

	r.addMissingUsers(client, missingOrgUsers)
	r.changeUsers(client, changeOrgUsers)

	for _, uc := range missingOrgUsers {
		delete(obsoleteOrgUsers, uc.email)
	}

	for _, uc := range changeOrgUsers {
		delete(obsoleteOrgUsers, email(uc.current.Email))
	}

	r.removeObsoleteUsers(client, obsoleteOrgUsers)

	return ctrl.Result{}, nil
}

func (r *OrganizationReconciler) findExpected(ctx context.Context, req ctrl.Request) (map[email]userConfig, error) {
	ol := &grafanav1.OrganizationList{}
	err := r.Client.List(
		ctx,
		ol,
		client.InNamespace(req.Namespace),
		client.MatchingFields{orgOwnerKey: req.Name},
	)
	if err != nil {
		return nil, err
	}

	expected := map[email]userConfig{}
	for _, org := range ol.Items {
		for _, u := range org.Spec.Users {
			expected[email(u.Email)] = userConfig{
				role: u.Role,
			}
		}
	}

	return expected, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OrganizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&grafanav1.Organization{}).
		Complete(r)
}

func (r *OrganizationReconciler) addMissingUsers(client *gapi.Client, users []missingUserConfig) {
	for _, uc := range users {
		err := client.AddOrgUser(1, string(uc.email), uc.role)
		if err != nil {
			panic(err)
		}
	}
}

func (r *OrganizationReconciler) changeUsers(client *gapi.Client, users []changeUserConfig) {
	for _, uc := range users {
		err := client.UpdateOrgUser(1, uc.current.UserID, uc.role)
		if err != nil {
			panic(err)
		}
	}
}

func (r *OrganizationReconciler) removeObsoleteUsers(client *gapi.Client, users map[email]gapi.OrgUser) {
	for _, uc := range users {
		err := client.RemoveOrgUser(1, uc.UserID)
		if err != nil {
			panic(err)
		}
	}
}
