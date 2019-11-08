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

package hub

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clustermanagerv1alpha1 "github.com/font/onprem/api/v1alpha1"
	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	yamlFilePath                    = "agent.yaml"
	OnPremCanonicalNamespace string = "onprem-hub-system"
	joinCommandTemplate      string = `# Run this on the hub cluster context
kubectl get secret %s -n %s -o=jsonpath="{.data.caBundle}" | base64 --decode > caBundle
kubectl get secret %s -n %s -o=jsonpath="{.data.token}" | base64 --decode > token
#Run this in the spoke cluster context, the spoke context is set by a path in SPOKE_KUBECONFIG env. var
export KUBECONFIG=${SPOKE_KUBECONFIG}
kubectl create namespace onprem-system
kubectl create secret generic hub-cluster -n onprem-system --from-file=caBundle --from-file=token
kubectl create configmap hub-cluster -n onprem-system --from-literal=joinClusterName=%s --from-literal=joinClusterNamespace=%s --from-literal=server=%s
cat << EOF | kubectl apply -f - 
%s
EOF
`
)

// RegisteredClusterReconciler reconciles a RegisteredCluster object
type RegisteredClusterReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

/*
We generally want to ignore (not requeue) NotFound errors, since we'll get a
reconciliation request once the object exists, and requeuing in the meantime
won't help.
*/
func ignoreNotFound(err error) error {
	if apierrs.IsNotFound(err) {
		return nil
	}
	return err
}

// +kubebuilder:rbac:groups=clustermanager.onprem.openshift.io,resources=registeredclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=clustermanager.onprem.openshift.io,resources=registeredclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;create;update;delete;watch
// +kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings,verbs=get;list;create;delete;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;create;delete;watch
// +kubebuilder:rbac:groups="config.openshift.io",resources=infrastructures,verbs=get;list;watch

func (r *RegisteredClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("registeredcluster", req.NamespacedName)
	var err error
	var registeredCluster clustermanagerv1alpha1.RegisteredCluster
	if err = r.Get(ctx, req.NamespacedName, &registeredCluster); err != nil {
		if apierrs.IsNotFound(err) {
			//handle delete of the RegisteredCluster CR
			log.Error(err, "Unable to get RegisteredCluster from the server")
			return ctrl.Result{}, ignoreNotFound(err)
		}
	}

	// handle finalizer
	// register a custom finalizer
	registeredClusterFinalizer := "storage.finalizers.onprem.openshift.io"

	// examine DeletionTimestamp to determine if object is under deletion
	if registeredCluster.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !containsString(registeredCluster.ObjectMeta.Finalizers, registeredClusterFinalizer) {
			registeredCluster.ObjectMeta.Finalizers = append(registeredCluster.ObjectMeta.Finalizers, registeredClusterFinalizer)
			if err = r.Update(context.Background(), &registeredCluster); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if containsString(registeredCluster.ObjectMeta.Finalizers, registeredClusterFinalizer) {
			// our finalizer is present, so lets handle any external dependency
			if err = r.deleteExternalResources(&req, &registeredCluster); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried
				return ctrl.Result{}, err
			}

			// remove our finalizer from the list and update it.
			registeredCluster.ObjectMeta.Finalizers = removeString(registeredCluster.ObjectMeta.Finalizers, registeredClusterFinalizer)
			if err = r.Update(context.Background(), &registeredCluster); err != nil {
				return ctrl.Result{}, err
			}
		}

		return ctrl.Result{}, err
	}

	//continue with the controller logic
	condition := registeredCluster.IsCondition(clustermanagerv1alpha1.ConditionTypeReadyToJoin)
	if registeredCluster.Status.ClusterAgentInfo != nil {
		//ready to join, check for staleness, disconnects
		sinceLastUpdate := time.Since(registeredCluster.Status.ClusterAgentInfo.LastUpdateTime.Time)
		if sinceLastUpdate >= registeredCluster.Spec.StaleDuration.Duration &&
			sinceLastUpdate < registeredCluster.Spec.DisconnectDuration.Duration {
			registeredCluster.SetCondition(clustermanagerv1alpha1.ConditionTypeAgentStale)
		} else if sinceLastUpdate > registeredCluster.Spec.DisconnectDuration.Duration {
			registeredCluster.SetCondition(clustermanagerv1alpha1.ConditionTypeAgentDisconnected)
		}

	} else if condition == nil {
		// not ready to join, create SA, rolebinding KubeConfig
		// set ServiceAccount and JoinCommand status subresource fields.
		serviceAccount, err := createServiceAccount(r, &req, &registeredCluster, log)
		if err != nil {
			return ctrl.Result{}, err
		}

		saSecret, err := getSecret(r, serviceAccount, log)
		if err != nil {
			log.Error(err, "Error getting the sa secret")
			return ctrl.Result{}, err
		}

		_, err = createRoleBinding(r, &req, &registeredCluster, log)
		if err != nil {
			return ctrl.Result{}, err
		}

		serverUrl, err := getServerUrl(r, log)
		if _, exists := saSecret.Data["service-ca.crt"]; exists {
			if _, exists := saSecret.Data["token"]; exists {
				joinSecret, err := createJoinSecret(r, saSecret.Data["service-ca.crt"], saSecret.Data["token"], registeredCluster.Name)
				if err != nil {
					return ctrl.Result{}, err
				}
				yamlFile, err := ioutil.ReadFile(yamlFilePath)
				if err != nil {
					log.Info("Cannot read yaml file from the deployment dir")
					return ctrl.Result{}, err
				}
				joinCommand := fmt.Sprintf(joinCommandTemplate, joinSecret.Name, joinSecret.Namespace,
					joinSecret.Name, joinSecret.Namespace, registeredCluster.Name, registeredCluster.Namespace, serverUrl, string(yamlFile))
				log.Info("Command output:", "joincommand", joinCommand)
				registeredCluster.Status.JoinCommand = &joinCommand

			} else {
				log.Info("Couldn't find the token key in the secret")
				return ctrl.Result{}, errors.New("Token key not found for the sa secret")
			}
		} else {
			log.Info("Couldn't find the service-ca.crt key in the secret")
			return ctrl.Result{}, errors.New("service-ca.crt not found in the secret")
		}
		// at this point we have a role binding created, now get the sa token and create
		// kubeconfig file.
		saName := serviceAccount.Name
		registeredCluster.Status.ServiceAccountName = &saName
		registeredCluster.SetCondition(clustermanagerv1alpha1.ConditionTypeReadyToJoin)
	}

	//update the status subresource now on the API server
	if err := r.Status().Update(ctx, &registeredCluster); err != nil {
		log.Error(err, "unable to update RegisteredCluster status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *RegisteredClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clustermanagerv1alpha1.RegisteredCluster{}).
		Complete(r)
}

func (r *RegisteredClusterReconciler) deleteExternalResources(req *ctrl.Request, j *clustermanagerv1alpha1.RegisteredCluster) error {
	// TODO: add finalizer code here
	err := deleteRoleBinding(r, req, j)
	if err != nil {
		return ignoreNotFound(err)
	}

	err = deleteJoinSecret(r, req, j)
	if err != nil {
		return ignoreNotFound(err)
	}

	return ignoreNotFound(deleteServiceAccount(r, req, j))
}

// Helper functions to check and remove string from a slice of strings.
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) (result []string) {
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return
}

func createServiceAccount(r *RegisteredClusterReconciler, req *ctrl.Request,
	registeredCluster *clustermanagerv1alpha1.RegisteredCluster, log logr.Logger) (*v1.ServiceAccount, error) {

	var saName string
	if registeredCluster.Spec.ServiceAccount != nil {
		saName = *registeredCluster.Spec.ServiceAccount
	} else {
		saName = fmt.Sprintf("%s-%s", registeredCluster.Name, "serviceaccount")
	}

	serviceAccount := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: req.Namespace,
		},
	}

	saObjectKey, err := client.ObjectKeyFromObject(serviceAccount)
	if err != nil {
		log.Error(err, "Error getting object key for service account", "name", saName)
		return nil, err
	}

	err = r.Get(context.Background(), saObjectKey, serviceAccount)
	switch {
	case apierrs.IsNotFound(err):
		err = r.Create(context.Background(), serviceAccount)
		switch {
		case apierrs.IsAlreadyExists(err):
			log.V(5).Info(fmt.Sprintf("Service Account %s/%s already exists", req.Namespace, saName))
			err = r.Get(context.Background(), saObjectKey, serviceAccount)
			if err != nil {
				log.Error(err, "Error getting service account object")
				return nil, err
			}
			return serviceAccount, nil
		case err != nil && !apierrs.IsAlreadyExists(err):
			return nil, err
		}
		return serviceAccount, nil
	case err != nil && !apierrs.IsNotFound(err):
		return nil, err
	}
	log.Info("Created service account")
	return serviceAccount, nil
}

func createRoleBinding(r *RegisteredClusterReconciler, req *ctrl.Request,
	registeredCluster *clustermanagerv1alpha1.RegisteredCluster, log logr.Logger) (*rbacv1.RoleBinding, error) {
	var saName string
	var roleBindingName string
	if registeredCluster.Spec.ServiceAccount != nil {
		saName = *registeredCluster.Spec.ServiceAccount
		roleBindingName = fmt.Sprintf("%s-%s", *registeredCluster.Spec.ServiceAccount, "rolebinding")
	} else {
		saName = fmt.Sprintf("%s-%s", registeredCluster.Name, "serviceaccount")
		roleBindingName = fmt.Sprintf("%s-%s", saName, "rolebinding")
	}
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleBindingName,
			Namespace: req.Namespace,
		},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: saName},
		},
		RoleRef: rbacv1.RoleRef{Kind: "ClusterRole",
			Name: "registeredcluster-role"},
	}
	rbObjectKey, err := client.ObjectKeyFromObject(roleBinding)
	if err != nil {
		log.Error(err, "Error getting object key for role binding", "name", roleBindingName)
		return nil, err
	}
	err = r.Get(context.Background(), rbObjectKey, roleBinding)
	switch {
	case apierrs.IsNotFound(err):
		err = r.Create(context.Background(), roleBinding)
		switch {
		case apierrs.IsAlreadyExists(err):
			log.V(5).Info(fmt.Sprintf("RoleBinding %s/%s already exists", req.Namespace, roleBindingName))
			err = r.Get(context.Background(), rbObjectKey, roleBinding)
			if err != nil {
				log.Error(err, "Error getting role binding object")
				return nil, err
			}
			return roleBinding, nil
		case err != nil && !apierrs.IsAlreadyExists(err):
			return nil, err
		}
		return roleBinding, nil
	case err != nil && !apierrs.IsNotFound(err):
		return nil, err
	}
	log.Info("Created role binding")
	return roleBinding, nil
}

func deleteServiceAccount(r *RegisteredClusterReconciler, req *ctrl.Request, j *clustermanagerv1alpha1.RegisteredCluster) error {
	var saName string
	if j.Spec.ServiceAccount != nil {
		saName = *j.Spec.ServiceAccount
	} else {
		saName = fmt.Sprintf("%s-%s", j.Name, "serviceaccount")
	}

	serviceAccount := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: req.Namespace,
		},
	}
	return r.Delete(context.Background(), serviceAccount)
}

func deleteRoleBinding(r *RegisteredClusterReconciler, req *ctrl.Request, j *clustermanagerv1alpha1.RegisteredCluster) error {
	var roleBindingName string
	if j.Spec.ServiceAccount != nil {
		roleBindingName = fmt.Sprintf("%s-%s", *j.Spec.ServiceAccount, "rolebinding")
	} else {
		roleBindingName = fmt.Sprintf("%s-%s-%s", j.Name, "serviceaccount", "rolebinding")
	}

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleBindingName,
			Namespace: req.Namespace,
		},
	}
	return r.Delete(context.Background(), roleBinding)
}

func getSecret(r *RegisteredClusterReconciler, serviceAccount *v1.ServiceAccount, log logr.Logger) (*v1.Secret, error) {
	var secretName string = ""
	if len(serviceAccount.Secrets) <= 0 {
		log.Info("No secrets are created yet for this service account")
		return nil, errors.New("Service account doesn't have any secrets")
	}
	for _, sec := range serviceAccount.Secrets {
		if strings.Contains(sec.Name, "-token-") {
			secretName = sec.Name
			log.Info("Found matching secret with name", "name", secretName)
			break
		}
	}
	if secretName == "" {
		log.Info("No matching secret found that can be used to get a token")
		return nil, errors.New("No secret found that has token in it")
	}
	log.Info("Now looking for a secret for this account with name", "name", secretName)
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: serviceAccount.Namespace,
		},
	}
	secretObjectKey, err := client.ObjectKeyFromObject(secret)
	if err != nil {
		log.Error(err, "Error getting object key for service account", "name", serviceAccount.Name)
		return nil, err
	}
	log.Info("Now getting the secret itself from the api server")
	err = r.Get(context.Background(), secretObjectKey, secret)
	if err != nil {
		log.Error(err, "Error getting secret from API server", "name", secret.Name)
		return nil, err
	}
	log.Info("Got secret with the desired secretobjectkey")
	if secret.Data == nil {
		log.Info("Secret is not ready yet")
		return nil, errors.New("Secret isn't populated yet with service-ca.crt and token")
	}
	log.Info("Returning created secret")
	return secret, nil

}

// This function creates a secret that saves the kubeconfig inside it. It doesn't return the actual updated secret object.
// But just the skeleton used to create it so it can be used for populating the JoinCommand.
func createJoinSecret(r *RegisteredClusterReconciler, caCert []byte, token []byte, registeredClusterName string) (*v1.Secret, error) {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", registeredClusterName, "join-secret"),
			Namespace: OnPremCanonicalNamespace,
		},
		Data: map[string][]byte{
			"caBundle": caCert,
			"token":    token,
		},
	}
	secretObjectKey, err := client.ObjectKeyFromObject(secret)
	if err != nil {
		return nil, err
	}

	err = r.Get(context.Background(), secretObjectKey, secret)
	if err == nil {
		return secret, err
	}
	if ignoreNotFound(err) != nil {
		return nil, err
	}
	err = r.Create(context.Background(), secret)
	if err != nil {
		return nil, err
	}

	return secret, nil
}

func deleteJoinSecret(r *RegisteredClusterReconciler, req *ctrl.Request, j *clustermanagerv1alpha1.RegisteredCluster) error {
	var secretName string = fmt.Sprintf("%s-%s", j.Name, "join-secret")

	joinSecret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: OnPremCanonicalNamespace,
		},
	}

	return r.Delete(context.Background(), joinSecret)
}

func getServerUrl(r *RegisteredClusterReconciler, log logr.Logger) (string, error) {
	infrastructure := &configv1.Infrastructure{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "",
		},
	}
	infraObjectKey, err := client.ObjectKeyFromObject(infrastructure)
	if err != nil {
		log.Error(err, "Error getting the object key for infrastructure object", "name", infrastructure.Name)
		return "", err
	}
	err = r.Get(context.Background(), infraObjectKey, infrastructure)
	if err != nil {
		log.Error(err, "Error getting infrastructure from API server", "name", infrastructure.Name)
		return "", err
	}
	return infrastructure.Status.APIServerURL, nil
}
