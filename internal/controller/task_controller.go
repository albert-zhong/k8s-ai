/*
Copyright 2024.

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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigscontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	aiv1alpha1 "github.com/albert-zhong/k8s-ai/api/v1alpha1"
	openai "github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"encoding/json"
)

// TaskReconciler reconciles a Task object
type TaskReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	OpenAIClient *openai.Client
}

//+kubebuilder:rbac:groups=*,resources=*,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Task object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileLog := log.FromContext(ctx)
	reconcileLog.Info("reconcile", "name", req.NamespacedName.Name, "namespace", req.NamespacedName.Namespace)

	task := &aiv1alpha1.Task{}
	if err := r.Get(ctx, req.NamespacedName, task); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, err
	}

	if task.Status.State != "" && task.Status.State != aiv1alpha1.InProgress {
		reconcileLog.Info("found failed or succeeded task", "name", req.NamespacedName.Name)
		return ctrl.Result{}, nil
	}

	newTask := task.DeepCopy()
	newTask.Status.Iteration = task.Status.Iteration + 1

	if task.Status.State == "" {
		newTask.Status.State = aiv1alpha1.InProgress
	}
	if task.Status.Iteration > 10 {
		reconcileLog.Info("task exceeded 10 iterations, stopping", "name", req.NamespacedName.Name)
		newTask.Status.State = aiv1alpha1.Failed
		return ctrl.Result{}, nil
	}

	chatCompletionReq, err := r.getChatCompletionRequest(ctx, task.Spec.Prompt)
	if err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, err
	}

	resp, err := r.OpenAIClient.CreateChatCompletion(ctx, *chatCompletionReq)
	if err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, err
	}

	var args PutKubernetesAPIArguments
	found := false
	for _, choice := range resp.Choices {
		for _, toolCall := range choice.Message.ToolCalls {
			if toolCall.Type != openai.ToolTypeFunction {
				continue
			}
			if toolCall.Function.Name != "put_kubernetes_api" {
				continue
			}
			reconcileLog.Info("found func call", "name", req.NamespacedName.Name)
			if err = json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
				return ctrl.Result{RequeueAfter: 5 * time.Minute}, err
			}
			found = true
			break
		}
	}
	if !found {
		reconcileLog.Info("not found", "name", req.NamespacedName.Name)
	}

	newTask.Status.HumanExplanation = args.HumanExplanation

	if args.Succeeded {
		reconcileLog.Info("task has succeeded, stopping", "name", req.NamespacedName.Name)
		newTask.Status.State = aiv1alpha1.Succeeded
	}

	fmt.Printf("put_kubernetes_api: %v\n", args)
	if err = r.PutKubernetesAPI(ctx, &args); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, err
	}

	if err := r.Status().Update(ctx, newTask); err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

type PutKubernetesAPIArguments struct {
	Data             map[string]interface{} `json:"data"`
	Uri              string                 `json:"uri"`
	HumanExplanation string                 `json:"human_explanation"`
	Succeeded        bool                   `json:"succeeded"`
	Verb             string                 `json:"verb"`
}

func (r *TaskReconciler) PutKubernetesAPI(ctx context.Context, args *PutKubernetesAPIArguments) error {
	apiServer := "https://kubernetes.default.svc"
	serviceAccountPath := "/var/run/secrets/kubernetes.io/serviceaccount"
	// namespacePath := serviceAccountPath + "/namespace"
	tokenPath := serviceAccountPath + "/token"
	caCertPath := serviceAccountPath + "/ca.crt"

	// Read the token
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return err
	}

	// Load CA cert
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		return err
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	// Setup HTTPS client
	tlsConfig := &tls.Config{
		RootCAs: caCertPool,
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	client := &http.Client{Transport: transport}

	// Create request
	reqUrl := apiServer + args.Uri

	argsDataJson, err := json.Marshal(args.Data)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(args.Verb, reqUrl, bytes.NewBuffer(argsDataJson))
	if err != nil {
		return err
	}

	// Add authorization header
	req.Header.Add("Authorization", "Bearer "+string(token))

	res, err := httputil.DumpRequest(req, true)
	if err != nil {
		return err
	}
	fmt.Println("REQUEST!!!")
	fmt.Print(string(res))

	// Make the request
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Read the response body
	bodyResp, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	fmt.Println(string(bodyResp))
	return nil
}

var baseContent = `You are k8s-ai, an LLM agent managing a Kubernetes cluster. You are extremely skilled in Kubernetes. Human users will submit task prompts in plain English. These are high-level requests like "figure out why my deployment is crash looping and fix it."

It is your responsibility to query the Kubernetes API to determine the issue, and possibly apply a set of Kubernetes templates to fulfill the task. Use the function call put_kubernetes_api.

If and only if the issue is already solved, you must apply an empty list of Kubernetes objects. If you user provides a prompt that is not well-defined (i.e it is not related to Kubernetes, just provide an empty list of Kubernetes objects, with an human explanation to put_kubernetes_api that the user provided a weird prompt.

Given the following conversation:
---
<list of objects currently on the Kubernetes cluster in JSON format>
<user prompt>
---
You MUST make a single call to put_kubernetes_api afterwards.
`

func (r *TaskReconciler) getChatCompletionRequest(ctx context.Context, prompt string) (*openai.ChatCompletionRequest, error) {
	namespacedOpts := []client.ListOption{
		client.InNamespace("default"),
	}
	opts := []client.ListOption{
		client.InNamespace(""), // This specifies all namespaces
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, namespacedOpts...); err != nil {
		return nil, err
	}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, opts...); err != nil {
		return nil, err
	}
	deployments := &appsv1.DeploymentList{}
	if err := r.List(ctx, deployments, namespacedOpts...); err != nil {
		return nil, err
	}
	replicaSets := &appsv1.ReplicaSetList{}
	if err := r.List(ctx, replicaSets, namespacedOpts...); err != nil {
		return nil, err
	}
	configMaps := &corev1.ConfigMapList{}
	if err := r.List(ctx, configMaps, namespacedOpts...); err != nil {
		return nil, err
	}
	var content strings.Builder
	content.WriteString(baseContent)
	podsJson, err := json.Marshal(pods)
	if err != nil {
		return nil, err
	}
	content.Write(podsJson)
	content.WriteByte('\n')
	nodesJson, err := json.Marshal(pods)
	if err != nil {
		return nil, err
	}
	content.Write(nodesJson)
	content.WriteByte('\n')
	deploymentsJson, err := json.Marshal(deployments)
	if err != nil {
		return nil, err
	}
	content.Write(deploymentsJson)
	content.WriteByte('\n')
	replicaSetsJson, err := json.Marshal(replicaSets)
	if err != nil {
		return nil, err
	}
	content.Write(replicaSetsJson)
	content.WriteByte('\n')
	configMapsJson, err := json.Marshal(configMaps)
	if err != nil {
		return nil, err
	}
	content.Write(configMapsJson)

	chatCompletionReq := &openai.ChatCompletionRequest{
		Model: openai.GPT4,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: content.String(),
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: prompt,
			},
		},
		Tools: []openai.Tool{
			{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        "put_kubernetes_api",
					Description: "Make a call to the Kubernetes API server of this cluster.",
					Parameters: jsonschema.Definition{
						Type: jsonschema.Object,
						Properties: map[string]jsonschema.Definition{
							"verb": {
								Type:        jsonschema.String,
								Description: "Kubernetes Verb, most likely POST",
							},
							"uri": {
								Type:        jsonschema.String,
								Description: "Kubernetes object URI",
							},
							"data": {
								Type:        jsonschema.Object,
								Description: "The application/json content to POST. This should be exactly the JSON template of the Kubernetes resource(s) to POST. Recall that you can apply a List of Kubernetes resources, do you don't need to make multiple put_kubernetes_api calls. You MUST create resources under the default namespace.",
							},
							"human_explanation": {
								Type:        jsonschema.String,
								Description: "Describe in concise, plain, and accurate plain language why you are putting this Kubernetes resource, and exactly how this solves the user's request.",
							},
							"succeeded": {
								Type:        jsonschema.Boolean,
								Description: "If the task is already complete or the user's request is out-of-scope (i.it is not actually about this Kubernetes cluster), set this property to true. This should be true if and only if the Kubernetes templates applied in the data property is empty.",
							},
						},
						Required: []string{"uri", "data", "human_explanation", "verb", "succeeded"},
					},
				},
			},
		},
	}
	return chatCompletionReq, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&aiv1alpha1.Task{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(sigscontroller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
