package bootstrap

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// KubeSecrets keeps Bootstrap's values in Kubernetes Secrets of one namespace.
type KubeSecrets struct {
	Client    kubernetes.Interface
	Namespace string
	// Labels put on Secrets it creates.
	Labels map[string]string
}

func (k KubeSecrets) Get(ctx context.Context, name string) (map[string][]byte, map[string]string, bool, error) {
	s, err := k.Client.CoreV1().Secrets(k.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	return s.Data, s.Annotations, true, nil
}

// Put creates the Secret, or replaces the data and annotations of an existing one.
func (k KubeSecrets) Put(ctx context.Context, name string, data map[string][]byte, annotations map[string]string) error {
	api := k.Client.CoreV1().Secrets(k.Namespace)
	cur, err := api.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: k.Namespace, Labels: k.Labels, Annotations: annotations},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if cur.Data == nil {
		cur.Data = map[string][]byte{}
	}
	for key, v := range data {
		cur.Data[key] = v
	}
	if len(annotations) > 0 && cur.Annotations == nil {
		cur.Annotations = map[string]string{}
	}
	for key, v := range annotations {
		cur.Annotations[key] = v
	}
	_, err = api.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}
