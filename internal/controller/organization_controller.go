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
		return fmt.Errorf("extracting user credentials failed: %w", err)
	}

	client, err := gapi.New(org.Spec.Url, gapi.Config{
		BasicAuth: ui,
	})
	if err != nil {
		return fmt.Errorf("creating Grafana Client failed: %w", err)
	}

	desired, err := r.findDesired(ctx, org)
	if err != nil {
		return fmt.Errorf("finding desired state failed: %w", err)
	}

	userActions, err := calculateUserBuckets(client, desired)
	if err != nil {
		return fmt.Errorf("calculating diff failed: %w", err)
	}

	err = r.addMissingUsers(client, userActions.missing)
	if err != nil {
		return fmt.Errorf("add missing users failed: %w", err)
	}
	err = r.changeUsers(client, userActions.change)
	if err != nil {
		return fmt.Errorf("changing present users failed: %w", err)
	}

	err = r.removeObsoleteUsers(client, userActions.obsolete)
	if err != nil {
		return fmt.Errorf("remove obsolete users failed: %w", err)
	}

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

	return url.UserPassword(string(username), string(password)), nil
}

func (r *OrganizationReconciler) findDesired(ctx context.Context, org *grafanav1.Organization) (map[email]userConfig, error) {

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
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *OrganizationReconciler) changeUsers(client *gapi.Client, users []changeUserConfig) error {
	wg := sync.WaitGroup{}
	wg.Add(len(users))
	errs := make([]error, len(users))
	for i, uc := range users {
		go func(i int, uc *changeUserConfig) {
			defer wg.Done()
			errs[i] = client.UpdateOrgUser(1, uc.current.UserID, uc.role)
		}(i, &uc)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

type orgUserId struct {
	OrgID  int64
	UserID int64
}

func (r *OrganizationReconciler) removeObsoleteUsers(client *gapi.Client, users map[email]gapi.OrgUser) error {
	uids := make([]orgUserId, 0, len(users))
	for _, uc := range users {
		uids = append(uids, orgUserId{
			OrgID:  uc.OrgID,
			UserID: uc.UserID,
		})
	}

	wg := sync.WaitGroup{}
	wg.Add(len(uids))
	errs := make([]error, len(uids))
	for i, uid := range uids {
		go func(i int, uid *orgUserId) {
			defer wg.Done()
			errs[i] = client.RemoveOrgUser(uid.OrgID, uid.UserID)
		}(i, &uid)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

type orgUserBuckets struct {
	missing  []missingUserConfig
	change   []changeUserConfig
	obsolete map[email]gapi.OrgUser
}

func calculateUserBuckets(client *gapi.Client, expected map[email]userConfig) (*orgUserBuckets, error) {
	currentOrgUsers, err := client.OrgUsersCurrent()
	if err != nil {
		return nil, err
	}

	users := &orgUserBuckets{
		obsolete: map[email]gapi.OrgUser{},
	}

	currentOrgUserMap := map[email]gapi.OrgUser{}

	for _, ou := range currentOrgUsers {
		currentOrgUserMap[email(ou.Email)] = ou
		users.obsolete[email(ou.Email)] = ou
	}

	for email, uc := range expected {
		alreadyPresentConfig, ok := currentOrgUserMap[email]
		if !ok {
			users.missing = append(users.missing, missingUserConfig{
				email:      email,
				userConfig: uc,
			})
			continue
		}

		if uc.role != alreadyPresentConfig.Role {
			users.change = append(users.change, changeUserConfig{
				userConfig: uc,
			})
		}
	}

	for _, uc := range users.missing {
		delete(users.obsolete, uc.email)
	}

	for _, uc := range users.change {
		delete(users.obsolete, email(uc.current.Email))
	}

	return users, nil
}
