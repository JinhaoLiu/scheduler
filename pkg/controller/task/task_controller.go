/*
Copyright 2019 X Code.

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

package task

import (
	"context"
	"fmt"
	"reflect"
	"time"

	kubeschedulingv1beta1 "github.com/scheduler/pkg/apis/kubescheduling/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller")

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new Task Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileTask{Client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("task-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to Task
	err = c.Watch(&source.Kind{Type: &kubeschedulingv1beta1.Task{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create
	// Uncomment watch a Deployment created by Task - change this for objects you create
	err = c.Watch(&source.Kind{Type: &appsv1.Deployment{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &kubeschedulingv1beta1.Task{},
	})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileTask{}

// ReconcileTask reconciles a Task object
type ReconcileTask struct {
	client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a Task object and makes changes based on the state read
// and what is in the Task.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  The scaffolding writes
// a Deployment as an example
// Automatically generate RBAC rules to allow the Controller to read and write Deployments
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubescheduling.k8s.io,resources=tasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubescheduling.k8s.io,resources=tasks/status,verbs=get;update;patch
func (r *ReconcileTask) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the Task instance
	instance := &kubeschedulingv1beta1.Task{}
	err := r.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	if instance.Status.Phase != "" {
		return reconcile.Result{}, nil
	}

	err = updateTaskPhase(instance, r, kubeschedulingv1beta1.TaskQueued)
	if err != nil {
		return reconcile.Result{}, err
	}

	uid := instance.Spec.UID
	budget, err := budgetWithUID(uid, request.Namespace, r)
	if err != nil {
		instance.Status.Message = "Task's user does not have budget"
		updateTaskPhase(instance, r, kubeschedulingv1beta1.TaskFailed)
		return reconcile.Result{}, err
	}
	log.Info(fmt.Sprintf("Found budget %v for uid %s", budget.Name, uid))

	tasks, err := allRunningTasksForUser(uid, request.Namespace, r)
	if err != nil {
		updateTaskPhase(instance, r, kubeschedulingv1beta1.TaskFailed)
		return reconcile.Result{}, err
	}

	var totalCostForAllTasks float64
	for _, task := range tasks {
		c, err := costWithName(task.Spec.Cost, request.Namespace, r)
		if err != nil {
			updateTaskPhase(instance, r, kubeschedulingv1beta1.TaskFailed)
			return reconcile.Result{}, err
		}
		log.Info(fmt.Sprintf("Found cost %v", c.Name))
		totalCostForAllTasks += totalCost(c)
	}

	costName := instance.Spec.Cost
	cost, err := costWithName(costName, request.Namespace, r)
	if err != nil {
		updateTaskPhase(instance, r, kubeschedulingv1beta1.TaskFailed)
		return reconcile.Result{}, err
	}
	log.Info(fmt.Sprintf("Found cost %v", cost.Name))

	budgetAmount := budget.Spec.Amount
	if budgetAmount < totalCostForAllTasks+totalCost(cost) {
		instance.Status.Message = "Task does not have enough budget"
		updateTaskPhase(instance, r, kubeschedulingv1beta1.TaskFailed)
		return reconcile.Result{}, fmt.Errorf("Task %v does not have enough budget", instance.Name)
	}

	err = updateTaskPhase(instance, r, kubeschedulingv1beta1.TaskReady)
	if err != nil {
		return reconcile.Result{}, err
	}

	go runTask(instance, r)

	// TODO(user): Change this to be the object type created by your controller
	// Define the desired Deployment object
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name + "-deployment",
			Namespace: instance.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"deployment": instance.Name + "-deployment"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"deployment": instance.Name + "-deployment"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "nginx",
						},
					},
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(instance, deploy, r.scheme); err != nil {
		return reconcile.Result{}, err
	}

	// TODO(user): Change this for the object type created by your controller
	// Check if the Deployment already exists
	found := &appsv1.Deployment{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: deploy.Name, Namespace: deploy.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		log.Info("Creating Deployment", "namespace", deploy.Namespace, "name", deploy.Name)
		err = r.Create(context.TODO(), deploy)
		return reconcile.Result{}, err
	} else if err != nil {
		return reconcile.Result{}, err
	}

	// TODO(user): Change this for the object type created by your controller
	// Update the found object and write the result back if there are any changes
	if !reflect.DeepEqual(deploy.Spec, found.Spec) {
		found.Spec = deploy.Spec
		log.Info("Updating Deployment", "namespace", deploy.Namespace, "name", deploy.Name)
		err = r.Update(context.TODO(), found)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

func allRunningTasksForUser(uid string, namespace string, r *ReconcileTask) ([]kubeschedulingv1beta1.Task, error) {
	taskList := &kubeschedulingv1beta1.TaskList{}
	err := r.List(context.Background(), client.InNamespace(namespace), taskList)
	if err != nil {
		return nil, err
	}

	tasks := []kubeschedulingv1beta1.Task{}
	for _, task := range taskList.Items {
		phase := task.Status.Phase
		if task.Spec.UID == uid && phase == kubeschedulingv1beta1.TaskInProgress {
			log.Info(fmt.Sprintf("Found running task %v for user %s", task.Name, uid))
			tasks = append(tasks, task)
		}
	}

	return tasks, nil
}

func totalCost(cost *kubeschedulingv1beta1.Cost) float64 {
	var priority float64
	var total float64
	for k, v := range cost.Spec.Resources {
		switch k {
		case kubeschedulingv1beta1.ResourcePriority:
			priority = v
		default:
			total += v
		}
	}

	total *= priority
	log.Info(fmt.Sprintf("Total amount for %v is %v", cost.Name, total))
	return total
}

func budgetWithUID(uid string, namespace string, r *ReconcileTask) (*kubeschedulingv1beta1.Budget, error) {
	budgets := &kubeschedulingv1beta1.BudgetList{}
	err := r.List(context.Background(), client.InNamespace(namespace), budgets)
	if err != nil {
		return nil, err
	}
	for _, budget := range budgets.Items {
		if budget.Spec.UID == uid {
			return &budget, nil
		}
	}

	return nil, fmt.Errorf("Unable to find uid %s in budgets", uid)
}

func costWithName(costName string, namespace string, r *ReconcileTask) (*kubeschedulingv1beta1.Cost, error) {
	costs := &kubeschedulingv1beta1.CostList{}
	err := r.List(context.Background(), client.InNamespace(namespace), costs)
	if err != nil {
		return nil, err
	}
	for _, cost := range costs.Items {
		if cost.Name == costName {
			return &cost, nil
		}
	}

	return nil, fmt.Errorf("Unable to find cost with name %s in budgets", costName)
}

func runTask(task *kubeschedulingv1beta1.Task, r *ReconcileTask) {
	log.Info("Start running task " + task.Name)
	err := updateTaskPhase(task, r, kubeschedulingv1beta1.TaskInProgress)
	if err != nil {
		return
	}
	// simulate running task
	time.Sleep(100 * time.Second)
	log.Info("task " + task.Name + " completed")
	updateTaskPhase(task, r, kubeschedulingv1beta1.TaskComplete)
}

func updateTaskPhase(task *kubeschedulingv1beta1.Task, r *ReconcileTask, phase kubeschedulingv1beta1.TaskPhase) error {
	task.Status.Phase = phase
	err := r.Update(context.Background(), task)
	if err != nil {
		log.Error(err, fmt.Sprintf("update Task %v to %v failed", task.Name, phase))
	}

	return err
}
