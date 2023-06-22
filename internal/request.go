package internal

import (
	"context"
	"fmt"
	"net/url"

	grafanav1 "github.com/abergmeier/grafana-org/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func BuildRequestUserInfo(ctx context.Context, client client.Client, namespace string, admin *grafanav1.GrafanaAdmin) (*url.Userinfo, error) {
	secret := &corev1.Secret{}

	name := types.NamespacedName{
		Namespace: namespace,
		Name:      admin.Username.ValueFrom.SecretKeyRef.Name,
	}
	err := client.Get(ctx, name, secret)
	if err != nil {
		return nil, fmt.Errorf("getting secret `%s` failed: %w", admin.Username.ValueFrom.SecretKeyRef.Name, err)
	}

	username := secret.Data[admin.Username.ValueFrom.SecretKeyRef.Key]

	name = types.NamespacedName{
		Namespace: namespace,
		Name:      admin.Password.ValueFrom.SecretKeyRef.Name,
	}
	err = client.Get(ctx, name, secret)
	if err != nil {
		return nil, fmt.Errorf("getting secret `%s` failed: %w", admin.Password.ValueFrom.SecretKeyRef.Name, err)
	}
	password := secret.Data[admin.Password.ValueFrom.SecretKeyRef.Key]

	return url.UserPassword(string(username), string(password)), nil
}
