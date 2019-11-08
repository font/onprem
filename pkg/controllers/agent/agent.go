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
	HeartBeatDelay = time.Second * 5
	kubeAPIQPS     = 20.0
	kubeAPIBurst   = 30

	defaultConfigMapName          = "hub-cluster"
	defaultSecretName             = "hub-cluster"
	apiEndpointKey                = "server"
	registeredClusterNameKey      = "joinClusterName"
	registeredClusterNamespaceKey = "joinClusterNamespace"
	caBundleKey                   = "caBundle"
	tokenKey                      = "token"
)

type RegisteredClusterCoordinates struct {
	APIEndpoint string
	Name        string
	Namespace   string
	Token       []byte
	CABundle    []byte
}

func GetRegisteredClusterCoordinates(spokeClient client.Client) (*RegisteredClusterCoordinates, error) {
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

	registeredClusterName, found := configMap.Data[registeredClusterNameKey]
	if !found || len(registeredClusterName) == 0 {
		return nil, errors.Errorf("The configmap for the hub cluster is missing a non-empty value for %q", registeredClusterNamespaceKey)
	}

	registeredClusterNamespace, found := configMap.Data[registeredClusterNamespaceKey]
	if !found || len(registeredClusterNamespace) == 0 {
		return nil, errors.Errorf("The configmap for the hub cluster is missing a non-empty value for %q", registeredClusterNamespaceKey)
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

	return &RegisteredClusterCoordinates{
		APIEndpoint: apiEndpoint,
		Name:        registeredClusterName,
		Namespace:   registeredClusterNamespace,
		Token:       token,
		CABundle:    caBundle,
	}, nil
}

func BuildHubClusterConfig(spokeClient client.Client, jcc *RegisteredClusterCoordinates) (*rest.Config, error) {
	// Build config using contents extracted from ConfigMap and Secret.
	clusterConfig, err := clientcmd.BuildConfigFromFlags(jcc.APIEndpoint, "")
	if err != nil {
		return nil, errors.Wrap(err, "Failed to build config using API endpoint")
	}

	clusterConfig.CAData = jcc.CABundle
	clusterConfig.BearerToken = string(jcc.Token)
	clusterConfig.QPS = float32(kubeAPIQPS)
	clusterConfig.Burst = kubeAPIBurst

	return clusterConfig, nil
}

// RegisteredClusterReconciler reconciles a RegisteredCluster object
type RegisteredClusterReconciler struct {
	HubClient   client.Client
	SpokeClient client.Client
	Log         logr.Logger
}

func (r *RegisteredClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("registeredcluster", req.NamespacedName)

	jcc, err := GetRegisteredClusterCoordinates(r.SpokeClient)
	if err != nil {
		log.Error(err, "unable to get registeredcluster coordinates")
		return ctrl.Result{}, err
	}

	// Ignore RegisteredClusters that do not belong to us. Only check name as we're
	// only watching one namespace.
	if jcc.Name != req.NamespacedName.Name {
		return ctrl.Result{}, nil
	}

	log.V(2).Info("reconciling")

	var registeredCluster v1alpha1.RegisteredCluster
	if err := r.HubClient.Get(ctx, req.NamespacedName, &registeredCluster); err != nil {
		log.Error(err, "unable to get registeredcluster")
		return ctrl.Result{}, err
	}

	reason := "AgentHeartBeat"
	message := "Spoke agent successfully connected to hub"
	agentConnected := v1alpha1.RegisteredClusterCondition{
		Type:    v1alpha1.ConditionTypeAgentConnected,
		Status:  v1alpha1.ConditionTrue,
		Reason:  &reason,
		Message: &message,
	}

	currentTime := metav1.Now()
	statusTransitioned := registeredCluster.Status.Conditions != nil && registeredCluster.Status.Conditions[0].Type != v1alpha1.ConditionTypeAgentConnected
	if statusTransitioned {
		agentConnected.LastTransitionTime = &currentTime
	}
	registeredCluster.Status.Conditions = []v1alpha1.RegisteredClusterCondition{agentConnected}
	agentInfo := &v1alpha1.ClusterAgentInfo{
		Version:        "v0.0.1",
		Image:          "quay.io/ifont/onprem-agent:latest",
		LastUpdateTime: currentTime,
	}
	registeredCluster.Status.ClusterAgentInfo = agentInfo

	if err := r.HubClient.Status().Update(ctx, &registeredCluster); err != nil {
		log.Error(err, "unable to update registeredcluster status")
		return ctrl.Result{}, err
	}

	// Force a reconcile even if no changes.
	return ctrl.Result{RequeueAfter: HeartBeatDelay}, nil
}

func (r *RegisteredClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.RegisteredCluster{}).
		Complete(r)
}
