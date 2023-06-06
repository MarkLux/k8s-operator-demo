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
	"sort"
	"time"

	"github.com/go-logr/logr"
	kbatch "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	batchv1 "marklux.cn/k8s-operator-demo/api/v1"
)

// some global vars
var (
	jobOwnerKey             = ".metadata.controller"
	apiGVStr                = batchv1.GroupVersion.String()
	scheduledTimeAnnotation = "batch.tutorial.kubebuilder.io/scheduled-at"
)

// CronJobReconciler reconciles a CronJob object
type CronJobReconciler struct {
	client.Client                      // k8s client (client-go)
	Scheme        *runtime.Scheme      // 对应type的scheme
	Clock                              // 用于mock时间
	Recorder      record.EventRecorder // 用于记录事件
}

//+kubebuilder:rbac:groups=batch.marklux.cn,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch.marklux.cn,resources=cronjobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch.marklux.cn,resources=cronjobs/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CronJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile
func (r *CronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Step 1. 获取事件对应的CornJob对象
	var cronJob batchv1.CronJob
	if err := r.Get(ctx, req.NamespacedName, &cronJob); err != nil {
		log.Error(err, "unable to fetch CronJob")
		// 如果没有找到对象，直接报错并返回空结果，结束处理
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Step 2. 查询CornJob对象下所有的Job List
	var childJobs kbatch.JobList

	// 根据索引获取所有的Job List
	// 我们在Setup中为controller runtime本地创建了一个索引缓存以便加速查询
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace),
		client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return ctrl.Result{}, err
	}

	// 对Job List进行分类
	var activeJobs []*kbatch.Job
	var successfulJobs []*kbatch.Job
	var failedJobs []*kbatch.Job
	var mostRecentTime *time.Time // 最后一次执行的时间

	for i, job := range childJobs.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "": // 还在执行中
			activeJobs = append(activeJobs, &childJobs.Items[i])
		case kbatch.JobFailed: // 执行失败
			failedJobs = append(failedJobs, &childJobs.Items[i])
		case kbatch.JobComplete: // 执行成功
			successfulJobs = append(successfulJobs, &childJobs.Items[i])
		}

		// 任务的启动时间存储在annotation中
		scheduledTimeForJob, err := getScheduledTimeForJob(&job)
		if err != nil {
			log.Error(err, "unable to parse schedule time for child job", "job", &job)
			continue
		}

		// 找到所有JobList中执行时间最靠后的一个
		if scheduledTimeForJob != nil {
			if mostRecentTime == nil {
				mostRecentTime = scheduledTimeForJob
			} else if mostRecentTime.Before(*scheduledTimeForJob) {
				mostRecentTime = scheduledTimeForJob
			}
		}
	}

	// 更新status - 最后一次执行时间
	if mostRecentTime != nil {
		cronJob.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		cronJob.Status.LastScheduleTime = nil
	}

	// 更新status - 所有active状态的Job列表
	cronJob.Status.Active = nil
	for _, activeJob := range activeJobs {
		jobRef, err := ref.GetReference(r.Scheme, activeJob)
		if err != nil {
			log.Error(err, "unable to make reference to active job", "job", activeJob)
			continue
		}
		cronJob.Status.Active = append(cronJob.Status.Active, *jobRef)
	}

	// DEBUG INFO
	log.V(1).Info("job count", "active jobs", len(activeJobs), "successful jobs", len(successfulJobs), "failed jobs", len(failedJobs))

	// 同步Status的更新到k8s
	if err := r.Status().Update(ctx, &cronJob); err != nil {
		log.Error(err, "unable to update CronJob status")
		return ctrl.Result{}, err
	}

	// Step 3. 清理历史运行的Job
	r.cleanUpHistoryJobs(ctx, cronJob, failedJobs, successfulJobs, log)

	// Step 4. 检查CronJob是否suspend了
	if cronJob.Spec.Suspend != nil && *cronJob.Spec.Suspend {
		// 如果已经suspend，直接放弃处理返回
		log.V(1).Info("cronjob suspended, skipping")
		r.Recorder.Event(&cronJob, "Normal", "Suspend", "cron job suspended.")
		return ctrl.Result{}, nil
	}

	// Step 5. 获取下一次执行时间
	missedRun, nextRun, err := getNextSchedule(&cronJob, r.Now())
	if err != nil {
		log.Error(err, "unable to figure out CronJob schedule")
		// we don't really care about requeuing until we get an update that
		// fixes the schedule, so don't return an error
		return ctrl.Result{}, nil
	}

	// 【NOTICE】REQUEUE RESULT
	scheduledResult := ctrl.Result{RequeueAfter: nextRun.Sub(r.Now())} // save this so we can re-use it elsewhere
	log = log.WithValues("now", r.Now(), "next run", nextRun)

	// Step 6. 判断是否要运行一个新的Job

	if missedRun.IsZero() {
		log.V(1).Info("no upcoming scheduled times, sleeping until next")
		return scheduledResult, nil
	}

	// make sure we're not too late to start the run
	log = log.WithValues("current run", missedRun)
	tooLate := false
	if cronJob.Spec.StartingDeadlineSeconds != nil {
		tooLate = missedRun.Add(time.Duration(*cronJob.Spec.StartingDeadlineSeconds) * time.Second).Before(r.Now())
	}
	if tooLate {
		log.V(1).Info("missed starting deadline for last run, sleeping till next")
		// TODO(directxman12): events
		return scheduledResult, nil
	}

	// 检查 ConcurrencyPolicy
	if cronJob.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent && len(activeJobs) > 0 {
		log.V(1).Info("concurrency policy blocks concurrent runs, skipping", "num active", len(activeJobs))
		return scheduledResult, nil
	}

	// ...or instruct us to replace existing ones...
	if cronJob.Spec.ConcurrencyPolicy == batchv1.ReplaceConcurrent {
		for _, activeJob := range activeJobs {
			// we don't care if the job was already deleted
			if err := r.Delete(ctx, activeJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete active job", "job", activeJob)
				return ctrl.Result{}, err
			}
		}
	}

	// 构建Job对象
	job, err := constructJobForCronJob(&cronJob, missedRun, r)
	if err != nil {
		log.Error(err, "unable to construct job from template")
		// don't bother requeuing until we get a change to the spec
		return scheduledResult, nil
	}

	// 应用到k8s cluster
	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "unable to create Job for CronJob", "job", job)
		return ctrl.Result{}, err
	}

	r.Recorder.Eventf(&cronJob, "Normal", "JobCreated", "new job instance %q created", job.Name)

	log.V(1).Info("created Job for CronJob run", "job", job)

	// Step 7. Requeue
	// we'll requeue once we see the running job, and update our status
	return scheduledResult, nil
}

func (r *CronJobReconciler) cleanUpHistoryJobs(
	ctx context.Context,
	cronJob batchv1.CronJob,
	failedJobs []*kbatch.Job,
	successfulJobs []*kbatch.Job,
	log logr.Logger,
) {
	// 清理失败的历史Job
	if cronJob.Spec.FailedJobsHistoryLimit != nil {
		// 按照开始时间排序
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i].Status.StartTime == nil {
				return failedJobs[j].Status.StartTime != nil
			}
			return failedJobs[i].Status.StartTime.Before(failedJobs[j].Status.StartTime)
		})
		// 删除Job
		for i, job := range failedJobs {
			if int32(i) >= int32(len(failedJobs))-*cronJob.Spec.FailedJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete old failed job", "job", job)
			} else {
				log.V(0).Info("deleted old failed job", "job", job)
			}
		}
	}

	// 清理成功的历史Job，同上
	if cronJob.Spec.SuccessfulJobsHistoryLimit != nil {
		sort.Slice(successfulJobs, func(i, j int) bool {
			if successfulJobs[i].Status.StartTime == nil {
				return successfulJobs[j].Status.StartTime != nil
			}
			return successfulJobs[i].Status.StartTime.Before(successfulJobs[j].Status.StartTime)
		})
		for i, job := range successfulJobs {
			if int32(i) >= int32(len(successfulJobs))-*cronJob.Spec.SuccessfulJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); (err) != nil {
				log.Error(err, "unable to delete old successful job", "job", job)
			} else {
				log.V(0).Info("deleted old successful job", "job", job)
			}
		}
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *CronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// set up a real clock, since we're not in a test
	if r.Clock == nil {
		r.Clock = realClock{}
	}

	// 创建一个indexer，用于根据CronJob的Owner获取Job
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kbatch.Job{}, jobOwnerKey, func(rawObj client.Object) []string {
		// grab the job object, extract the owner...
		job := rawObj.(*kbatch.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		// ...make sure it's a CronJob...
		if owner.APIVersion != apiGVStr || owner.Kind != "CronJob" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.CronJob{}).
		Owns(&kbatch.Job{}).
		Complete(r)
}
