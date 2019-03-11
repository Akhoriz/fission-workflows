package invocation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fission/fission-workflows/pkg/api/store"
	"github.com/golang/protobuf/ptypes"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"

	"github.com/fission/fission-workflows/pkg/api"
	"github.com/fission/fission-workflows/pkg/api/aggregates"
	"github.com/fission/fission-workflows/pkg/api/events"
	"github.com/fission/fission-workflows/pkg/controller"
	"github.com/fission/fission-workflows/pkg/controller/expr"
	"github.com/fission/fission-workflows/pkg/fes"
	"github.com/fission/fission-workflows/pkg/scheduler"
	"github.com/fission/fission-workflows/pkg/util/gopool"
)

const (
	maxParallelExecutions = 1000
	Name                  = "invocation"
)

var (
	log = logrus.WithField("component", "controller.invocation")

	// workflow-related metrics
	invocationStatus = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "workflows",
		Subsystem: "controller_invocation",
		Name:      "status",
		Help:      "Count of the different statuses of workflow invocations.",
	}, []string{"status"})

	invocationDuration = prometheus.NewSummary(prometheus.SummaryOpts{
		Namespace: "workflows",
		Subsystem: "controller_invocation",
		Name:      "finished_duration",
		Help:      "Duration of an invocation from start to a finished state.",
		Objectives: map[float64]float64{
			0:     0.0001,
			0.001: 0.0001,
			0.01:  0.0001,
			0.02:  0.0001,
			0.1:   0.0001,
			0.25:  0.0001,
			0.5:   0.0001,
			0.75:  0.0001,
			0.9:   0.0001,
			0.98:  0.0001,
			0.99:  0.0001,
			0.999: 0.0001,
			1:     0.0001,
		},
	})

	exprEvalDuration = prometheus.NewSummary(prometheus.SummaryOpts{
		Namespace: "workflows",
		Subsystem: "controller_invocation",
		Name:      "expr_eval_duration",
		Help:      "Duration of the evaluation of the input expressions.",
	})
)

func init() {
	prometheus.MustRegister(invocationStatus, invocationDuration, exprEvalDuration)
}

type Controller struct {
	invocations   *store.Invocations
	workflows     *store.Workflows
	taskAPI       *api.Task
	invocationAPI *api.Invocation
	stateStore    *expr.Store
	scheduler     *scheduler.InvocationScheduler
	cancelFn      context.CancelFunc
	evalPolicy    controller.Rule
	evalStore     *controller.EvalStore
	workQueue     workqueue.RateLimitingInterface
	workerPool    *gopool.GoPool
	executor      *controller.LocalExecutor
}

func NewController(invocations *store.Invocations, workflows *store.Workflows, workflowScheduler *scheduler.InvocationScheduler,
	taskAPI *api.Task, invocationAPI *api.Invocation, stateStore *expr.Store, executor *controller.LocalExecutor) *Controller {
	ctr := &Controller{
		invocations:   invocations,
		workflows:     workflows,
		scheduler:     workflowScheduler,
		taskAPI:       taskAPI,
		invocationAPI: invocationAPI,
		stateStore:    stateStore,
		evalStore:     &controller.EvalStore{},
		workQueue:     workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		workerPool:    gopool.New(maxParallelExecutions),
		executor:      executor,
	}

	ctr.evalPolicy = defaultPolicy(ctr)
	return ctr
}

func (cr *Controller) Init(sctx context.Context) error {
	ctx, cancelFn := context.WithCancel(sctx)
	cr.cancelFn = cancelFn

	// Subscribe to invocation creations and task events.
	go func(ctx context.Context) {
		sub := cr.invocations.GetInvocationUpdates()
		if sub == nil {
			log.Warn("Invocation cache does not support pubsub.")
			return
		}
		for {
			select {
			case msg := <-sub.Ch:
				update, err := sub.ToNotification(msg)
				if err != nil {
					log.Warnf("Failed to convert pubsub message to notification: %v", err)
				}
				err = cr.Notify(update)
				if err != nil {
					log.Errorf("Failed to notify controller of update: %v", err)
				}
			case <-ctx.Done():
				err := sub.Close()
				if err != nil {
					log.Error(err)
				}
				log.Info("Notification listener stopped.")
				return
			}
		}
	}(ctx)

	// process evaluation queue
	go wait.Until(cr.runWorker(sctx), time.Second, sctx.Done())

	return nil
}

func (cr *Controller) Notify(update *fes.Notification) error {
	log.WithFields(logrus.Fields{
		"notification": update.Event,
		"labels":       update.Labels(),
	}).Debugf("Controller event: %v", update.Event)
	entity, err := store.ParseNotificationToInvocation(update)
	if err != nil {
		return err
	}

	if es, ok := cr.evalStore.Load(entity.ID()); ok {
		es.Span().LogKV("event", fmt.Sprintf("%s - %v", update.Event, update.Labels()))
	}
	switch update.Event.Type {
	case events.EventInvocationCompleted:
		cr.finishAndDeleteEvalState(entity.ID(), true, "completion reason: "+events.EventInvocationCompleted)
	case events.EventInvocationCanceled:
		cr.finishAndDeleteEvalState(entity.ID(), false, "completion reason: "+events.EventInvocationCanceled)
	case events.EventInvocationFailed:
		cr.finishAndDeleteEvalState(entity.ID(), false, "completion reason: "+events.EventInvocationFailed)
	case events.EventTaskFailed:
		fallthrough
	case events.EventTaskSucceeded:
		fallthrough
	case events.EventInvocationCreated:
		spanCtx, err := fes.ExtractTracingFromEvent(update.Event)
		if err != nil {
			logrus.Warn("Failed to extract opentracing metadata from event %v", update.Event.Id)
		}
		es, _ := cr.evalStore.LoadOrStore(entity.ID(), spanCtx)
		cr.workQueue.Add(es)
	default:
		log.Debugf("Controller ignored event type: %v", update.Event)
	}
	return nil
}

func (cr *Controller) Tick(tick uint64) error {
	// Short loop: invocations the controller is actively tracking
	var err error
	if tick%10 == 0 {
		err = cr.checkEvalStore()
	}

	// Long loop: to check if there are any orphans
	if tick%50 == 0 {
		err = cr.checkModelCaches()
	}

	return err
}

func (cr *Controller) checkEvalStore() error {
	for id, es := range cr.evalStore.List() {
		// Check if the EvalState is not yet finished
		if es.IsFinished() {
			continue
		}

		last, ok := es.Last()
		if !ok {
			continue
		}

		// Check if the EvalState is not already in progress
		select {
		case <-es.Lock():
			reevaluateAt := last.Timestamp.Add(time.Duration(100) * time.Millisecond)
			if time.Now().UnixNano() > reevaluateAt.UnixNano() {
				controller.EvalRecovered.WithLabelValues(Name, "evalStore").Inc()
				log.Debugf("Adding missing invocation %v to the queue", id)
				cr.workQueue.Add(es)
			}
			es.Free()
		default:
			// EvalState is already in progress
		}
	}
	return nil
}

// checkCaches iterates over the current caches submitting evaluation for invocation when needed
func (cr *Controller) checkModelCaches() error {
	// Short control loop
	entities := cr.invocations.List()
	for _, entity := range entities {
		// Ignore those that are in the evalStore; those will get picked up by checkEvalStore.
		if _, ok := cr.evalStore.Load(entity.Id); ok {
			continue
		}

		wi := aggregates.NewWorkflowInvocation(entity.Id)
		err := cr.invocations.Get(wi)
		if err != nil {
			log.Errorf("Failed to read '%v' from cache: %v.", wi.Aggregate(), err)
			continue
		}

		if !wi.Status.Finished() {
			// TODO grab the span context from the model / events
			span := opentracing.GlobalTracer().StartSpan("/controller/recoverFromModelCache")
			controller.EvalRecovered.WithLabelValues(Name, "cache").Inc()
			es, _ := cr.evalStore.LoadOrStore(wi.ID(), span.Context())
			cr.workQueue.Add(es)
			span.Finish()
		}
	}
	return nil
}

func (cr *Controller) Evaluate(invocationID string) {
	start := time.Now()
	// Fetch and attempt to claim the evaluation
	evalState, ok := cr.evalStore.Load(invocationID)
	if !ok {
		log.Warnf("Skipping evaluation of unknown invocation: %v", invocationID)
		return
	}
	select {
	case <-evalState.Lock():
		defer evalState.Free()
	default:
		log.Debugf("Failed to obtain access to invocation %s", invocationID)
		controller.EvalJobs.WithLabelValues(Name, "duplicate").Inc()
		return
	}
	log.Debugf("Evaluating invocation %s", invocationID)

	// Check if there are no tasks left to be executed
	if taskCount := cr.executor.GetGroupTasks(invocationID); taskCount > 0 {
		log.Debugf("ignoring %v - there are still %d tasks left to be executed", invocationID, taskCount)
		return
	}

	// Fetch the workflow invocation for the provided invocation id
	wfi, err := cr.invocations.GetInvocation(invocationID)
	// TODO move to rule
	if err != nil && wfi == nil {
		log.Errorf("controller failed to get invocation for invocation id '%s': %v", invocationID, err)
		controller.EvalJobs.WithLabelValues(Name, "error").Inc()
		return
	}
	// TODO move to rule
	if wfi.Status.Finished() {
		log.Debugf("No need to evaluate finished invocation %v", invocationID)
		controller.EvalJobs.WithLabelValues(Name, "error").Inc()
		evalState.Finish(true, "finished")
		return
	}

	// Fetch the workflow relevant to the invocation
	if wfi.Workflow() == nil {
		wf, err := cr.workflows.GetWorkflow(wfi.GetSpec().GetWorkflowId())
		// TODO move to rule
		if err != nil && wf == nil {
			log.Errorf("controller failed to get workflow '%s' for invocation id '%s': %v", wfi.Spec.WorkflowId,
				invocationID, err)
			controller.EvalJobs.WithLabelValues(Name, "error").Inc()
			return
		}
		if !wf.GetStatus().Ready() {
			log.Errorf("Workflow '%s' is not ready", wfi.Spec.WorkflowId)
			controller.EvalJobs.WithLabelValues(Name, "error").Inc()
			go func() { // TODO fix this
				time.Sleep(100 * time.Millisecond)
				cr.workQueue.Add(evalState)
			}()
			return
		}
		wfi.Spec.Workflow = wf
	}

	// Evaluate invocation
	record := controller.NewEvalRecord() // TODO implement rulepath + cause

	ec := NewEvalContext(evalState, wfi)

	actions := cr.evalPolicy.Eval(ec)
	//record.Action = action
	if actions == nil {
		controller.EvalJobs.WithLabelValues(Name, "noop").Inc()
		return
	}

	// Execute action
	for _, action := range actions {
		cr.executor.Submit(&controller.DefaultTask{
			GroupID: wfi.ID(),
			Apply:   action.Apply,
		})
	}
	controller.EvalJobs.WithLabelValues(Name, "action").Inc()

	// Record this evaluation
	evalState.Record(record)

	// Record statistics
	controller.EvalDuration.
		WithLabelValues(Name, fmt.Sprintf("%T", actions)).
		Observe(float64(time.Now().Sub(start)))
	if wfi.GetStatus().Finished() {
		cr.finishAndDeleteEvalState(wfi.ID(), true, "")
	}
	invocationStatus.WithLabelValues(wfi.GetStatus().GetStatus().String()).Inc()
}

func (cr *Controller) Close() error {
	ctx, cancelFn := context.WithTimeout(context.Background(), time.Minute)
	defer cancelFn()
	err := cr.workerPool.GracefulStop(ctx)
	if err != nil {
		log.Debugf("Failed to gracefully stop pool: %v", err)
	}
	cr.evalStore.Close()
	cr.cancelFn()
	return nil
}

func (cr *Controller) createFailAction(invocationID string, err error) controller.Action {
	return &ActionFail{
		API:          cr.invocationAPI,
		InvocationID: invocationID,
		Err:          err,
	}
}

func (cr *Controller) runWorker(ctx context.Context) func() {
	return func() {
		for cr.processNextItem(ctx, cr.workerPool) {
			// continue looping
		}
	}
}

func (cr *Controller) processNextItem(ctx context.Context, pool *gopool.GoPool) bool {
	key, quit := cr.workQueue.Get()
	if quit {
		return false
	}
	defer cr.workQueue.Done(key)

	es := key.(*controller.EvalState)

	err := pool.Submit(ctx, func() {
		controller.EvalQueueSize.WithLabelValues("invocation").Dec()
		cr.Evaluate(es.ID())
	})
	if err != nil {
		if err == gopool.ErrPoolClosed {
			return false
		}
		log.Errorf("failed to submit invocation %v for execution: %v", es.ID(), err)
	}

	// No error, reset the ratelimit counters
	cr.workQueue.Forget(key)

	return true
}

func (cr *Controller) finishAndDeleteEvalState(evalStateID string, success bool, msg string) {
	var start time.Time
	es, ok := cr.evalStore.Load(evalStateID)
	if !ok {
		return
	}
	wfi, _ := cr.invocations.GetInvocation(evalStateID)
	if wfi != nil {
		start, _ = ptypes.Timestamp(wfi.GetMetadata().GetCreatedAt())
	}
	es.Finish(success, msg)
	cr.evalStore.Delete(evalStateID)
	cr.stateStore.Delete(evalStateID)
	cr.workQueue.Forget(es)
	log.Debugf("Removed entity %v from eval state", evalStateID)
	if wfi != nil {
		invocationDuration.Observe(float64(time.Now().Sub(start)))
	}
}

func defaultPolicy(ctr *Controller) controller.Rule {
	return &controller.RuleEvalUntilAction{
		Rules: []controller.Rule{
			&controller.RuleTimedOut{
				OnTimedOut: &ActionFail{
					API: ctr.invocationAPI,
					Err: errors.New("timed out"),
				},
				Timeout: time.Duration(10) * time.Minute,
			},

			&controller.RuleExceededErrorCount{
				OnExceeded: &ActionFail{
					API: ctr.invocationAPI,
				},
				MaxErrorCount: 0,
			},
			&RuleCheckIfCompleted{
				InvocationAPI: ctr.invocationAPI,
			},

			&RuleWorkflowIsReady{
				InvocationAPI: ctr.invocationAPI,
			},

			&RuleSchedule{
				Scheduler:     ctr.scheduler,
				InvocationAPI: ctr.invocationAPI,
				FunctionAPI:   ctr.taskAPI,
				StateStore:    ctr.stateStore,
			},
		},
	}
}
