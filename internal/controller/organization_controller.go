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
	"encoding/base64"
	"fmt"
	"net/url"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	grafanav1 "github.com/abergmeier/grafana-org/api/v1"
	gapi "github.com/grafana/grafana-api-golang-client"
	corev1 "k8s.io/api/core/v1"
)

// OrganizationReconciler reconciles a Organization object
type OrganizationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

//+kubebuilder:rbac:groups=grafana.abergmeier.github.io,resources=organizations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=grafana.abergmeier.github.io,resources=organizations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=grafana.abergmeier.github.io,resources=organizations/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *OrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	org := &grafanav1.Organization{}
	err := r.Client.Get(ctx, req.NamespacedName, org)
	if err != nil {
		return ctrl.Result{
			RequeueAfter: time.Second,
		}, err
	}

	err = r.reconcileGrafanaOrganization(ctx, req, org)
	if err != nil {
		return ctrl.Result{
			RequeueAfter: time.Second,
		}, err
	}

	return ctrl.Result{}, nil
}

func (r *OrganizationReconciler) reconcileGrafanaOrganization(ctx context.Context, req ctrl.Request, org *grafanav1.Organization) error {

	ui, err := r.buildUserInfo(ctx, req, org)
	if err != nil {
		return err
	}

	client, err := gapi.New(org.Spec.Url, gapi.Config{
		BasicAuth: ui,
	})
	if err != nil {
		return err
	}

	expected, err := r.findExpected(ctx, org)
	if err != nil {
		return err
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

	err = r.addMissingUsers(client, missingOrgUsers)
	if err != nil {
		return err
	}
	r.changeUsers(client, changeOrgUsers)

	for _, uc := range missingOrgUsers {
		delete(obsoleteOrgUsers, uc.email)
	}

	for _, uc := range changeOrgUsers {
		delete(obsoleteOrgUsers, email(uc.current.Email))
	}

	r.removeObsoleteUsers(client, obsoleteOrgUsers)

	return nil
}

func (r *OrganizationReconciler) buildUserInfo(ctx context.Context, req ctrl.Request, org *grafanav1.Organization) (*url.Userinfo, error) {
	secret := &corev1.Secret{}

	name := types.NamespacedName{
		Namespace: req.Namespace,
		Name:      org.Spec.Admin.Username.ValueFrom.SecretKeyRef.Name,
	}
	err := r.Client.Get(ctx, name, secret)
	if err != nil {
		return nil, fmt.Errorf("getting secret `%s` failed: %w", org.Spec.Admin.Username.ValueFrom.SecretKeyRef.Name, err)
	}

	username := secret.Data[org.Spec.Admin.Username.ValueFrom.SecretKeyRef.Key]

	name = types.NamespacedName{
		Namespace: req.Namespace,
		Name:      org.Spec.Admin.Password.ValueFrom.SecretKeyRef.Name,
	}
	err = r.Client.Get(ctx, name, secret)
	if err != nil {
		return nil, fmt.Errorf("getting secret `%s` failed: %w", org.Spec.Admin.Password.ValueFrom.SecretKeyRef.Name, err)
	}
	password := secret.Data[org.Spec.Admin.Password.ValueFrom.SecretKeyRef.Key]

	username, err = base64.StdEncoding.DecodeString(string(username))
	if err != nil {
		return nil, fmt.Errorf("decoding username failed: %w", err)
	}

	password, err = base64.StdEncoding.DecodeString(string(password))
	if err != nil {
		return nil, fmt.Errorf("decoding password failed: %w", err)
	}

	return url.UserPassword(string(username), string(password)), nil
}

func (r *OrganizationReconciler) findExpected(ctx context.Context, org *grafanav1.Organization) (map[email]userConfig, error) {

	expected := map[email]userConfig{}

	for _, u := range org.Spec.Users {
		expected[email(u.Email)] = userConfig{
			role: u.Role,
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

func (r *OrganizationReconciler) addMissingUsers(client *gapi.Client, users []missingUserConfig) error {
	wg := sync.WaitGroup{}
	wg.Add(len(users))
	errs := make([]error, len(users))
	for i, uc := range users {
		go func(i int, uc *missingUserConfig) {
			defer wg.Done()
			errs[i] = client.AddOrgUser(1, string(uc.email), uc.role)
		}(i, &uc)
	}
	wg.Wait()
	for _, err := range errs {
		return err
	}
	return nil
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
