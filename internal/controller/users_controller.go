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
	"math/rand"
	"sort"
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

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// UsersReconciler reconciles a Users object
type UsersReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=grafana.abergmeier.github.io,resources=users,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=grafana.abergmeier.github.io,resources=users/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=grafana.abergmeier.github.io,resources=users/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *UsersReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	org := &grafanav1.Users{}
	err := r.Client.Get(ctx, req.NamespacedName, org)
	if err != nil {
		return ctrl.Result{
			RequeueAfter: time.Second,
		}, err
	}

	err = r.reconcileGrafanaUsers(ctx, req, org)
	if err != nil {
		return ctrl.Result{
			RequeueAfter: time.Second,
		}, err
	}

	return ctrl.Result{}, nil
}

func (r *UsersReconciler) reconcileGrafanaUsers(ctx context.Context, req ctrl.Request, users *grafanav1.Users) error {

	ui, err := internal.BuildRequestUserInfo(ctx, r.Client, req.Namespace, users.Spec.Admin)
	if err != nil {
		return fmt.Errorf("extracting user credentials failed: %w", err)
	}

	client, err := gapi.New(users.Spec.Url, gapi.Config{
		BasicAuth: ui,
	})
	if err != nil {
		return fmt.Errorf("creating Grafana Client failed: %w", err)
	}

	desiredState, err := r.buildDesiredState(ctx, users)
	if err != nil {
		return fmt.Errorf("finding desired state failed: %w", err)
	}

	userActions, err := calculateUserBuckets(client, ui.Username(), desiredState)
	if err != nil {
		return fmt.Errorf("calculating diff failed: %w", err)
	}

	err = r.createMissingUsers(ctx, client, userActions.missing)
	if err != nil {
		return fmt.Errorf("add missing users failed: %w", err)
	}
	err = r.updateCurrentUsers(ctx, client, userActions.change)
	if err != nil {
		return fmt.Errorf("changing present users failed: %w", err)
	}

	err = r.deleteObsoleteUsers(ctx, client, userActions.obsoleteIds)
	if err != nil {
		return fmt.Errorf("remove obsolete users failed: %w", err)
	}

	return nil
}

func (r *UsersReconciler) buildDesiredState(ctx context.Context, users *grafanav1.Users) (map[email]grafanav1.User, error) {

	expected := map[email]grafanav1.User{}

	for _, u := range users.Spec.Users {
		expected[email(u.Email)] = u
	}

	return expected, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *UsersReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&grafanav1.Users{}).
		Complete(r)
}

type createUserError struct {
	error
	Email email
}

func (err *createUserError) Error() string {
	return fmt.Sprintf("adding User (email: %s) failed: %s", err.Email, err.error)
}

func (r *UsersReconciler) createMissingUsers(ctx context.Context, client *gapi.Client, users []gapi.User) error {
	logger := log.FromContext(ctx)

	wg := sync.WaitGroup{}
	wg.Add(len(users))
	errs := make([]*createUserError, len(users))
	for i, u := range users {
		go func(i int, u gapi.User) {
			defer wg.Done()

			// We need to have a password since HTTP API is enforcing it
			if u.Password == "" {
				u.Password = randStringBytes(16)
			}

			_, err := client.CreateUser(u)
			if err != nil {
				errs[i] = &createUserError{
					error: err,
					Email: email(u.Email),
				}
				return
			}
			logger.Info("Added User", "email", u.Email)
		}(i, u)
	}
	wg.Wait()
	unreturned := filterNilCreateUserErrors(errs)
	if len(unreturned) >= 1 {
		for _, err := range unreturned[1:] {
			logger.Error(err.error, "adding User failed", "email", err.Email)
		}
		return unreturned[0]
	}
	return nil
}

type updateUserError struct {
	error
	Email  string
	UserID int64
}

func (r *UsersReconciler) updateCurrentUsers(ctx context.Context, client *gapi.Client, users []gapi.User) error {
	logger := log.FromContext(ctx)

	wg := sync.WaitGroup{}
	wg.Add(len(users))
	errs := make([]*updateUserError, len(users))
	for i, u := range users {
		go func(i int, u gapi.User) {
			defer wg.Done()
			err := client.UserUpdate(u)
			if err != nil {
				errs[i] = &updateUserError{
					error:  err,
					Email:  u.Email,
					UserID: u.ID,
				}
				return
			}
			logger.Info("Updated User", "email", u.Email)
		}(i, u)
	}
	wg.Wait()
	unreturned := filterNilUpdateUserErrors(errs)
	if len(unreturned) >= 1 {
		for _, err := range unreturned[1:] {
			logger.Error(err.error, "changing User failed", "userid", err.UserID, "email", err.Email)
		}
		return fmt.Errorf("changing User in Organization failed: %w", unreturned[0])
	}
	return nil
}

type deleteUserError struct {
	error
	UserID int64
}

func (r *UsersReconciler) deleteObsoleteUsers(ctx context.Context, client *gapi.Client, userIds []int64) error {
	logger := log.FromContext(ctx)

	wg := sync.WaitGroup{}
	wg.Add(len(userIds))
	errs := make([]*deleteUserError, len(userIds))
	for i, uid := range userIds {
		go func(i int, uid int64) {
			defer wg.Done()
			err := client.DeleteUser(uid)
			if err != nil {
				errs[i] = &deleteUserError{
					error:  err,
					UserID: uid,
				}
				return
			}
			logger.Info("Deleted User", "id", uid)
		}(i, uid)
	}
	wg.Wait()
	unreturned := filterNilDeleteUserErrors(errs)
	if len(unreturned) >= 1 {
		for _, err := range unreturned[1:] {
			logger.Error(err.error, "deleting User failed", "userid", err.UserID)
		}
		return fmt.Errorf("deleting User failed: %w", unreturned[0])
	}
	return nil
}

type userBuckets struct {
	missing     []gapi.User
	change      []gapi.User
	obsoleteIds []int64
}

func calculateUserBuckets(client *gapi.Client, adminLogin string, desired map[email]grafanav1.User) (*userBuckets, error) {
	currentUsers, err := client.Users()
	if err != nil {
		return nil, fmt.Errorf("fetching current Users failed: %w", err)
	}

	users := &userBuckets{}

	currentUserMap := map[email]gapi.User{}

	obsoleteIds := make(map[int64]struct{}, len(currentUsers))

	for _, ou := range currentUsers {
		// Excempt from changing admin
		// Grafana has no strategy to handle these cases properly
		if ou.Login == adminLogin || ou.Email == adminLogin {
			continue
		}
		currentUserMap[email(ou.Email)] = gapi.User{
			ID:         ou.ID,
			Email:      ou.Email,
			Name:       ou.Name,
			Login:      ou.Login,
			AuthLabels: ou.AuthLabels,
		}
		obsoleteIds[ou.ID] = struct{}{}
	}

	for email, uc := range desired {
		alreadyPresentUser, ok := currentUserMap[email]
		if !ok {
			users.missing = append(users.missing, gapi.User{
				Email:      string(email),
				Name:       uc.Name,
				Login:      uc.Login,
				AuthLabels: uc.AuthLabels,
			})
			continue
		}

		delete(obsoleteIds, alreadyPresentUser.ID)

		if needsChange(&uc, &alreadyPresentUser) {
			changed := alreadyPresentUser
			changed.Email = uc.Email
			changed.Name = uc.Name
			changed.Login = uc.Login
			changed.AuthLabels = uc.AuthLabels
			users.change = append(users.change, changed)
		}
	}

	for id := range obsoleteIds {
		users.obsoleteIds = append(users.obsoleteIds, id)
	}

	return users, nil
}

func needsChange(lhs *grafanav1.User, current *gapi.User) bool {
	if len(lhs.AuthLabels) != len(current.AuthLabels) {
		return true
	}

	if len(lhs.AuthLabels) != 0 {
		sort.Strings(lhs.AuthLabels)
	}

	if len(current.AuthLabels) != 0 {
		sort.Strings(current.AuthLabels)
	}

	for i := 0; i != len(lhs.AuthLabels); i++ {
		if lhs.AuthLabels[i] != current.AuthLabels[i] {
			return true
		}
	}

	return lhs.Email != current.Email || lhs.Login != current.Login || lhs.Name != current.Name
}

func filterNilCreateUserErrors(errs []*createUserError) []*createUserError {
	filtered := make([]*createUserError, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	return filtered
}

func filterNilUpdateUserErrors(errs []*updateUserError) []*updateUserError {
	filtered := make([]*updateUserError, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	return filtered
}

func filterNilDeleteUserErrors(errs []*deleteUserError) []*deleteUserError {
	filtered := make([]*deleteUserError, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	return filtered
}

func randStringBytes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}
