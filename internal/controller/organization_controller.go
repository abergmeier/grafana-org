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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	grafanav1 "github.com/abergmeier/grafana-org/api/v1"
	"github.com/abergmeier/grafana-org/internal"
	gapi "github.com/grafana/grafana-api-golang-client"
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
	gapi.User
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

	ui, err := internal.BuildRequestUserInfo(ctx, r.Client, req.Namespace, org.Spec.Admin)
	if err != nil {
		return fmt.Errorf("extracting user credentials failed: %w", err)
	}

	client, err := gapi.New(org.Spec.Url, gapi.Config{
		BasicAuth: ui,
	})
	if err != nil {
		return fmt.Errorf("creating Grafana Client failed: %w", err)
	}

	desiredState, err := r.buildDesiredState(ctx, org)
	if err != nil {
		return fmt.Errorf("finding desired state failed: %w", err)
	}

	userActions, err := calculateOrgUserBuckets(client, ui.Username(), desiredState)
	if err != nil {
		return fmt.Errorf("calculating diff failed: %w", err)
	}

	err = r.addMissingUsers(ctx, client, userActions.missing)
	if err != nil {
		return fmt.Errorf("add missing users failed: %w", err)
	}
	err = r.changeUsers(ctx, client, userActions.change)
	if err != nil {
		return fmt.Errorf("changing present users failed: %w", err)
	}

	err = r.removeObsoleteUsers(ctx, client, userActions.obsoleteIds)
	if err != nil {
		return fmt.Errorf("remove obsolete users failed: %w", err)
	}

	return nil
}

func (r *OrganizationReconciler) buildDesiredState(ctx context.Context, org *grafanav1.Organization) (map[email]grafanav1.OrganizationUser, error) {

	expected := map[email]grafanav1.OrganizationUser{}

	for _, u := range org.Spec.Users {
		expected[email(u.Email)] = u
	}

	return expected, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *OrganizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&grafanav1.Organization{}).
		Complete(r)
}

type orgUserAddError struct {
	error
	Email email
	OrgID int64
}

func (err *orgUserAddError) Error() string {
	return fmt.Sprintf("adding User (email: %s) to Organization (id: %d) failed: %s", err.Email, err.OrgID, err.error)
}

func (r *OrganizationReconciler) addMissingUsers(ctx context.Context, client *gapi.Client, users []missingUserConfig) error {
	logger := log.FromContext(ctx)

	wg := sync.WaitGroup{}
	wg.Add(len(users))
	errs := make([]*orgUserAddError, len(users))
	for i, uc := range users {
		go func(i int, uc *missingUserConfig) {
			defer wg.Done()

			_, err := client.UserByEmail(string(uc.Email))
			if err != nil {
				// Need to have a user first to try to add to Org
				return
			}

			err = client.AddOrgUser(uc.OrgID, string(uc.Email), uc.role)
			if err != nil {
				errs[i] = &orgUserAddError{
					error: err,
					Email: email(uc.Email),
					OrgID: uc.OrgID,
				}
				return
			}
			logger.Info("Added User to Organization", "orgid", uc.OrgID, "email", uc.Email)
		}(i, &uc)
	}
	wg.Wait()
	unreturned := filterNilOrgUserAddErrors(errs)
	if len(unreturned) >= 1 {
		for _, err := range unreturned[1:] {
			logger.Error(err.error, "adding User to Organization failed", "orgid", err.OrgID, "email", err.Email)
		}
		return unreturned[0]
	}
	return nil
}

type orgUserError struct {
	error
	orgUserId
	Email string
}

func (err *orgUserError) Error() string {
	return fmt.Sprintf("operation on User (email: %d) in Organization (id: %d) failed: %s", err.Email, err.OrgID, err.error)
}

func (r *OrganizationReconciler) changeUsers(ctx context.Context, client *gapi.Client, users []gapi.OrgUser) error {
	logger := log.FromContext(ctx)

	wg := sync.WaitGroup{}
	wg.Add(len(users))
	errs := make([]*orgUserError, len(users))
	for i, ou := range users {
		go func(i int, ou gapi.OrgUser) {
			defer wg.Done()
			err := client.UpdateOrgUser(ou.OrgID, ou.UserID, ou.Role)
			if err != nil {
				errs[i] = &orgUserError{
					error: err,
					orgUserId: orgUserId{
						OrgID:  ou.OrgID,
						UserID: ou.UserID,
					},
					Email: ou.Email,
				}
				return
			}
			logger.Info("Updated User in Organization", "orgid", ou.OrgID, "email", ou.Email)
		}(i, ou)
	}
	wg.Wait()
	unreturned := filterNilOrgUserErrors(errs)
	if len(unreturned) >= 1 {
		for _, err := range unreturned[1:] {
			logger.Error(err.error, "updating User in Organization failed", "orgid", err.OrgID, "email", err.Email)
		}
		return fmt.Errorf("updating User in Organization failed: %w", unreturned[0])
	}
	return nil
}

type orgUserId struct {
	OrgID  int64
	UserID int64
}

func (r *OrganizationReconciler) removeObsoleteUsers(ctx context.Context, client *gapi.Client, userIds []orgUserId) error {
	logger := log.FromContext(ctx)

	wg := sync.WaitGroup{}
	wg.Add(len(userIds))
	errs := make([]*orgUserError, len(userIds))
	for i, uid := range userIds {
		go func(i int, uid orgUserId) {
			defer wg.Done()
			err := client.RemoveOrgUser(uid.OrgID, uid.UserID)
			if err != nil {
				errs[i] = &orgUserError{
					error:     err,
					orgUserId: uid,
				}
				return
			}
			logger.Info("Removed User from Organization", "orgid", uid.OrgID, "userid", uid.UserID)
		}(i, uid)
	}
	wg.Wait()
	unreturned := filterNilOrgUserErrors(errs)
	if len(unreturned) >= 1 {
		for _, err := range unreturned[1:] {
			logger.Error(err.error, "removing User from Organization failed", "orgid", err.OrgID, "userid", err.UserID)
		}
		return fmt.Errorf("removing User from Organization failed: %w", unreturned[0])
	}
	return nil
}

type orgUserBuckets struct {
	missing     []missingUserConfig
	change      []gapi.OrgUser
	obsoleteIds []orgUserId
}

func calculateOrgUserBuckets(client *gapi.Client, adminLogin string, desired map[email]grafanav1.OrganizationUser) (*orgUserBuckets, error) {
	currentOrgUsers, err := client.OrgUsersCurrent()
	if err != nil {
		return nil, err
	}

	users := &orgUserBuckets{}

	obsoleteByEmail := map[string]gapi.OrgUser{}

	currentOrgUserMap := map[email]gapi.OrgUser{}

	for _, ou := range currentOrgUsers {
		// Excempt from changing admin
		// Grafana has no strategy to handle these cases properly
		if ou.Login == adminLogin || ou.Email == adminLogin {
			continue
		}
		currentOrgUserMap[email(ou.Email)] = ou
		obsoleteByEmail[ou.Email] = ou
	}

	for email, uc := range desired {
		alreadyPresentUser, ok := currentOrgUserMap[email]
		if !ok {
			users.missing = append(users.missing, missingUserConfig{
				User: gapi.User{
					Email: string(email),
					OrgID: 1,
				},
				userConfig: userConfig{
					role: uc.Role,
				},
			})
			continue
		}

		delete(obsoleteByEmail, uc.Email)

		if uc.Role != alreadyPresentUser.Role {
			changed := alreadyPresentUser
			changed.Role = uc.Role
			users.change = append(users.change, changed)
		}
	}

	for _, u := range obsoleteByEmail {
		users.obsoleteIds = append(users.obsoleteIds, orgUserId{
			OrgID:  u.OrgID,
			UserID: u.UserID,
		})
	}

	return users, nil
}

func filterNilOrgUserErrors(errs []*orgUserError) []*orgUserError {
	filtered := make([]*orgUserError, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	return filtered
}

func filterNilOrgUserAddErrors(errs []*orgUserAddError) []*orgUserAddError {
	filtered := make([]*orgUserAddError, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	return filtered
}
