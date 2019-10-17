/*

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

package agent

import (
	"context"
	"io/ioutil"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/font/onprem/api/v1alpha1"
)

var (
	kubeAPIQPS     = 20.0
	kubeAPIBurst   = 30
	HeartBeatDelay = time.Second * 5

	defaultConfigMapName    = "hub-cluster"
	defaultSecretName       = "hub-cluster"
	apiEndpointKey          = "server"
	joinClusterNameKey      = "joinClusterName"
	joinClusterNamespaceKey = "joinClusterNamespace"
	caBundleKey             = "cabundle"
	tokenKey                = "token"
)

func BuildHubClusterConfig(spokeClient client.Client) (*rest.Config, error) {
	namespaceBytes, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get my namespace")
	}
	namespace := string(namespaceBytes)

	// Extract contents from ConfigMap.
	namespacedName := client.ObjectKey{Namespace: namespace, Name: defaultConfigMapName}
	configMap := &corev1.ConfigMap{}
	err = spokeClient.Get(context.TODO(), namespacedName, configMap)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get configmap %+v", namespacedName)
	}

	apiEndpoint, found := configMap.Data[apiEndpointKey]
	if !found || len(apiEndpoint) == 0 {
		return nil, errors.Errorf("The configmap for the hub cluster is missing a non-empty value for %q", apiEndpointKey)
	}

	joinClusterName, found := configMap.Data[joinClusterNameKey]
	if !found || len(joinClusterName) == 0 {
		return nil, errors.Errorf("The configmap for the hub cluster is missing a non-empty value for %q", joinClusterNamespaceKey)
	}

	joinClusterNamespace, found := configMap.Data[joinClusterNamespaceKey]
	if !found || len(joinClusterNamespace) == 0 {
		return nil, errors.Errorf("The configmap for the hub cluster is missing a non-empty value for %q", joinClusterNamespaceKey)
	}

	// Extract contents from Secret.
	namespacedName.Name = defaultSecretName
	secret := &corev1.Secret{}
	err = spokeClient.Get(context.TODO(), namespacedName, secret)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get secret %+v", namespacedName)
	}

	token, found := secret.Data[tokenKey]
	if !found || len(token) == 0 {
		return nil, errors.Errorf("The secret for the hub cluster is missing a non-empty value for %q", tokenKey)
	}

	caBundle, found := secret.Data[caBundleKey]
	if !found || len(caBundle) == 0 {
		return nil, errors.Errorf("The secret for the hub cluster is missing a non-empty value for %q", caBundleKey)
	}

	// Build config using contents extracted from ConfigMap and Secret.
	clusterConfig, err := clientcmd.BuildConfigFromFlags(apiEndpoint, "")
	if err != nil {
		return nil, errors.Wrap(err, "Failed to build config using API endpoint")
	}

	clusterConfig.CAData = caBundle
	clusterConfig.BearerToken = string(token)
	clusterConfig.QPS = float32(kubeAPIQPS)
	clusterConfig.Burst = kubeAPIBurst

	return clusterConfig, nil
}

// JoinedClusterReconciler reconciles a JoinedCluster object
type JoinedClusterReconciler struct {
	HubClient   client.Client
	SpokeClient client.Client
	Log         logr.Logger
}

func (r *JoinedClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("joinedcluster", req.NamespacedName)
	log.V(2).Info("reconciling")

	var joinedCluster v1alpha1.JoinedCluster
	if err := r.HubClient.Get(ctx, req.NamespacedName, &joinedCluster); err != nil {
		log.Error(err, "unable to get joinedcluster")
		return ctrl.Result{}, err
	}

	reason := "AgentHeartBeat"
	message := "Spoke agent successfully connected to hub"
	agentConnected := v1alpha1.JoinedClusterConditions{
		Type:    v1alpha1.ConditionTypeAgentConnected,
		Status:  v1alpha1.ConditionTrue,
		Reason:  &reason,
		Message: &message,
	}

	currentTime := metav1.Now()
	statusTransitioned := joinedCluster.Status.Conditions != nil && joinedCluster.Status.Conditions[0].Type != v1alpha1.ConditionTypeAgentConnected
	if statusTransitioned {
		agentConnected.LastTransitionTime = &currentTime
	}
	joinedCluster.Status.Conditions = []v1alpha1.JoinedClusterConditions{agentConnected}
	joinedCluster.Status.ClusterAgentInfo.Version = "v0.0.1"
	joinedCluster.Status.ClusterAgentInfo.Image = "172.17.0.1:5000/onprem-agent:latest"
	joinedCluster.Status.ClusterAgentInfo.LastUpdateTime = currentTime

	// Force a reconcile even if no changes.
	return ctrl.Result{RequeueAfter: HeartBeatDelay}, nil
}

func (r *JoinedClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.JoinedCluster{}).
		Complete(r)
}
