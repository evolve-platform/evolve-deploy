package aws

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/image"
	"github.com/evolve-platform/evolve-deploy/internal/logging"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// How long the tool waits for ECS to reach the gate, and for the rollout to
// finish once it has been answered.
//
// A PAUSE hook may wait up to 14 days on the ECS side, so nothing here is
// racing the platform: these bound the pipeline, not the deployment.
const (
	gateTimeout   = 20 * time.Minute
	finishTimeout = 30 * time.Minute
	pollInterval  = 10 * time.Second
)

// bgPayload is what a blue-green ECS change carries from Plan to Apply.
type bgPayload struct {
	// register is the task definition to create, as on the direct path.
	register *ecs.RegisterTaskDefinitionInput
	// previous is the task definition the service runs now.
	previous string

	// deployment is the service deployment Stage started, and hook the paused
	// lifecycle hook it is waiting at. Both are read by Switch and Abandon.
	deployment string
	hook       string
	// staged is the task definition ARN this release registered.
	staged string
}

// Routable reports which target types carry traffic.
//
// A lambda beside an ECS service has no listener rule and nothing to shift: it
// rides along and is written at the switch.
func (d *Driver) Routable(t config.TargetType) bool {
	return t == config.TypeECS
}

// Pointable is false, and that is the honest answer rather than a limitation.
//
// A release does move this service's traffic, so it is routable. But ECS owns
// both target groups, swaps them itself, and terminates the old tasks once the
// bake time is up — so between releases there is no second side standing that
// anything could be pointed at. See Point for what to do instead.
func (d *Driver) Pointable(config.TargetType) bool { return false }

// Fallback: ECS terminates the old side itself, so what a rollback would find
// is a window if bake_time asked for one, and otherwise nothing.
func (d *Driver) Fallback(t *config.Target) string {
	if bake := t.Strategy.Bake(); bake > 0 {
		return "for " + bake.String()
	}
	return "gone"
}

// Sides reports which version is serving and which one a release is going to.
//
// ⚠️ This is where ECS is genuinely a different shape, and the difference is
// worth stating rather than papering over. On Container Apps and Cloud Run the
// two sides are the tool's own: it names them, it owns the weights, and which
// one is live alternates every release. On ECS the platform owns both target
// groups and swaps them itself. There is no label to read back and nothing to
// point at by name.
//
// So the sides here are roles, not identities: the first label is whatever is
// serving and the second is whatever this release is bringing up, every time.
// `{{.label}}` and `{{.previous_label}}` still mean what they say — the side
// this release deploys to, and what a rollback returns to — because those were
// named after their role in the release for exactly this reason. What does not
// work is anything that assumes the roles alternate, which is why
// planECSBlueGreen refuses `strategy.env` here.
func (d *Driver) Sides(ctx context.Context, t *config.Target) (*target.Sides, error) {
	svc, err := d.describeService(ctx, t)
	if err != nil {
		return nil, err
	}
	if err := checkBlueGreenService(t, svc); err != nil {
		return nil, err
	}

	labels := t.Strategy.Labels
	if len(labels) != 2 {
		return nil, fmt.Errorf("strategy.labels needs exactly two names, got %d", len(labels))
	}

	sides := &target.Sides{
		Active: target.Side{Label: labels[0], Revision: awssdk.ToString(svc.TaskDefinition)},
		Idle:   target.Side{Label: labels[1]},
	}
	if sides.Active.Revision != "" {
		if td, err := d.describeTaskDef(ctx, sides.Active.Revision); err == nil {
			name, err := target.PickContainer(
				containerNames(td.ContainerDefinitions), t.Container, ecsAppContainer)
			if err == nil {
				sides.Active.Version = currentImageTag(td, name)
			}
		} else {
			slog.Debug("could not read the serving task definition", "service", t.Name,
				"taskDefinition", sides.Active.Revision, "err", err)
		}
	}
	return sides, nil
}

// checkBlueGreenService refuses a service Terraform has not set up for this.
//
// Routing on ECS is real infrastructure — two target groups, two listener
// rules, a role that lets ECS move traffic between them — and the tool reads it
// rather than writing it. Every one of these is a thing that has to exist
// before a release can be staged, so finding out at the switch would be finding
// out far too late.
func checkBlueGreenService(t *config.Target, svc *ecstypes.Service) error {
	var missing []string

	if c := svc.DeploymentController; c == nil || c.Type != ecstypes.DeploymentControllerTypeEcs {
		got := "unset"
		if c != nil {
			got = string(c.Type)
		}
		return fmt.Errorf(
			"ecs service %s uses the %s deployment controller; blue-green needs ECS\n"+
				"    (set deployment_controller { type = \"ECS\" } in Terraform — the older\n"+
				"    CODE_DEPLOY controller is a different mechanism with an appspec)",
			t.Name, got)
	}

	var adv *ecstypes.AdvancedConfiguration
	for _, lb := range svc.LoadBalancers {
		if lb.AdvancedConfiguration != nil {
			adv = lb.AdvancedConfiguration
			break
		}
	}
	if adv == nil {
		return fmt.Errorf(
			"ecs service %s has no load balancer with an advanced configuration, so\n"+
				"    there is nothing to shift traffic between. Blue-green on ECS needs an\n"+
				"    alternate target group, a production listener rule, a test listener rule\n"+
				"    and a role, all declared on the service in Terraform.", t.Name)
	}
	if awssdk.ToString(adv.AlternateTargetGroupArn) == "" {
		missing = append(missing, "alternate_target_group_arn")
	}
	if awssdk.ToString(adv.ProductionListenerRule) == "" {
		missing = append(missing, "production_listener_rule")
	}
	if awssdk.ToString(adv.TestListenerRule) == "" {
		missing = append(missing, "test_listener_rule")
	}
	if awssdk.ToString(adv.RoleArn) == "" {
		missing = append(missing, "role_arn")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf(
			"ecs service %s is missing %s on its load balancer's advanced configuration;\n"+
				"    Terraform owns those and the tool never writes them",
			t.Name, strings.Join(missing, ", "))
	}
	return nil
}

// Stage registers the new task definition, hands ECS the whole rollout, and
// waits at the gate ECS offers.
//
// This is the half of blue-green that looks least like the other clouds and
// behaves most like them. There is no weight to write: the tool declares
// BLUE_GREEN with a PAUSE hook and ECS does the scale-up and the test traffic
// shift itself. What comes back is the same thing Container Apps and Cloud Run
// return — a staged side with an address, serving no production traffic.
func (d *Driver) Stage(ctx context.Context, ch *target.Change) (*target.Staged, error) {
	p := ch.Payload.(*bgPayload)
	t := ch.Target

	timer := logging.Start("register task definition", "family", t.Name)
	registered, err := d.ecs.RegisterTaskDefinition(ctx, p.register)
	if err != nil {
		return nil, fmt.Errorf("register task definition: %w", err)
	}
	p.staged = awssdk.ToString(registered.TaskDefinition.TaskDefinitionArn)
	timer.Done("arn", p.staged)

	// The gate is a PAUSE hook at POST_TEST_TRAFFIC_SHIFT: test traffic fully
	// on the new side, production traffic still entirely on the old one. That
	// is exactly where a smoke test belongs, and it is the reason this fits the
	// same choreography at all.
	//
	// PAUSE takes no Lambda and no appspec. The pipeline is the hook.
	cfg := &ecstypes.DeploymentConfiguration{
		Strategy:          ecstypes.DeploymentStrategyBlueGreen,
		BakeTimeInMinutes: awssdk.Int32(int32(t.Strategy.Bake().Minutes())),
		LifecycleHooks: []ecstypes.DeploymentLifecycleHook{{
			TargetType: ecstypes.DeploymentLifecycleHookTargetTypePause,
			LifecycleStages: []ecstypes.DeploymentLifecycleHookStage{
				ecstypes.DeploymentLifecycleHookStagePostTestTrafficShift,
			},
		}},
	}

	start := logging.Start("start blue-green deployment", "cluster", t.Cluster,
		"service", t.Name, "taskDefinition", p.staged)
	if _, err := d.ecs.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster:                 awssdk.String(t.Cluster),
		Service:                 awssdk.String(t.Name),
		TaskDefinition:          awssdk.String(p.staged),
		DeploymentConfiguration: cfg,
	}); err != nil {
		return nil, fmt.Errorf("update service: %w", err)
	}
	start.Done()

	if err := d.awaitGate(ctx, ch, p); err != nil {
		return nil, err
	}

	return &target.Staged{
		Label:    ch.Sides.Idle.Label,
		Revision: p.staged,
		URL:      t.TestURL,
	}, nil
}

// awaitGate blocks until the deployment is paused and waiting to be told to go
// on, and records what to answer.
func (d *Driver) awaitGate(ctx context.Context, ch *target.Change, p *bgPayload) error {
	t := ch.Target
	started := time.Now()
	deadline := started.Add(gateTimeout)

	timer := logging.Start("waiting for the deployment to reach the gate",
		"cluster", t.Cluster, "service", t.Name, "timeout", gateTimeout)

	for {
		dep, err := d.latestDeployment(ctx, t)
		if err != nil {
			// A read that fails is never a verdict: the deployment was started
			// moments ago and ECS may still be catching up.
			slog.Debug("could not read the service deployment",
				"service", t.Name, "err", err)
		} else if dep != nil {
			p.deployment = awssdk.ToString(dep.ServiceDeploymentArn)

			switch dep.Status {
			case ecstypes.ServiceDeploymentStatusRollbackRequested,
				ecstypes.ServiceDeploymentStatusRollbackInProgress,
				ecstypes.ServiceDeploymentStatusRollbackSuccessful,
				ecstypes.ServiceDeploymentStatusRollbackFailed:
				// ECS decided by itself — a failing health check, or an alarm
				// declared in Terraform. There is nothing left to gate.
				return fmt.Errorf("%s: ECS rolled the deployment back before the gate: %s",
					t.Name, orNone(awssdk.ToString(dep.StatusReason)))
			case ecstypes.ServiceDeploymentStatusStopped,
				ecstypes.ServiceDeploymentStatusStopRequested:
				return fmt.Errorf("%s: the deployment was stopped: %s",
					t.Name, orNone(awssdk.ToString(dep.StatusReason)))
			}

			if hook := pendingHook(dep); hook != "" {
				p.hook = hook
				timer.Done("elapsed", time.Since(started).Round(time.Second),
					"stage", string(dep.LifecycleStage))
				return nil
			}
		}

		if time.Now().After(deadline) {
			stage := "an unknown stage"
			if dep != nil && dep.LifecycleStage != "" {
				stage = string(dep.LifecycleStage)
			}
			return fmt.Errorf("%s: the deployment did not reach the gate within %s; "+
				"it is at %s", t.Name, gateTimeout, stage)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// pendingHook names the paused hook, if the deployment is waiting at one.
func pendingHook(dep *ecstypes.ServiceDeployment) string {
	if dep.LifecycleStage != ecstypes.ServiceDeploymentLifecycleStagePostTestTrafficShift {
		return ""
	}
	for _, h := range dep.LifecycleHookDetails {
		if h.Status != ecstypes.DeploymentLifecycleHookStatusAwaitingAction {
			continue
		}
		if h.TargetType != ecstypes.DeploymentLifecycleHookTargetTypePause {
			continue
		}
		return awssdk.ToString(h.HookId)
	}
	return ""
}

// Switch tells ECS to go on, and waits for production traffic to have moved.
func (d *Driver) Switch(ctx context.Context, ch *target.Change) error {
	return d.answer(ctx, ch, ecstypes.DeploymentLifecycleHookActionContinue)
}

// Abandon tells ECS to roll back the deployment that never served production
// traffic.
//
// This is the path that makes a failed blue-green deploy a non-event, and on
// ECS it is the platform's own rollback rather than a traffic write of ours:
// test traffic goes back, the new tasks are torn down, and nothing anyone saw
// has happened.
func (d *Driver) Abandon(ctx context.Context, ch *target.Change) error {
	p := ch.Payload.(*bgPayload)
	if p.hook == "" {
		// Never reached the gate, so there is nothing paused to answer. ECS
		// tears down a deployment that failed before that on its own.
		return nil
	}
	return d.answer(ctx, ch, ecstypes.DeploymentLifecycleHookActionRollback)
}

func (d *Driver) answer(
	ctx context.Context,
	ch *target.Change,
	action ecstypes.DeploymentLifecycleHookAction,
) error {
	p := ch.Payload.(*bgPayload)
	if p.deployment == "" || p.hook == "" {
		return fmt.Errorf("%s: there is no paused deployment to answer", ch.Target.Name)
	}

	timer := logging.Start("answering the deployment gate",
		"service", ch.Target.Name, "action", string(action))
	if _, err := d.ecs.ContinueServiceDeployment(ctx, &ecs.ContinueServiceDeploymentInput{
		ServiceDeploymentArn: awssdk.String(p.deployment),
		HookId:               awssdk.String(p.hook),
		Action:               action,
	}); err != nil {
		return fmt.Errorf("%s: answering the gate with %s: %w", ch.Target.Name, action, err)
	}
	timer.Done()

	// The hook is spent either way, so a retry must not answer it twice.
	p.hook = ""
	return d.awaitFinish(ctx, ch.Target.Name, p.deployment, wantRollback(action))
}

// wantRollback reads a hook action as the outcome it is asking for.
func wantRollback(action ecstypes.DeploymentLifecycleHookAction) bool {
	return action == ecstypes.DeploymentLifecycleHookActionRollback
}

// awaitFinish waits for the deployment to reach a resting state and reads that
// state as the verdict.
//
// `rollingBack` is what was asked for, and the two success states are checked
// against it rather than accepted on their own. A deployment that reports
// SUCCESSFUL after a rollback was requested has not done what it was told, and
// silently reading that as fine would leave the wrong version serving with a
// green line above it.
func (d *Driver) awaitFinish(
	ctx context.Context,
	name, deployment string,
	rollingBack bool,
) error {
	deadline := time.Now().Add(finishTimeout)
	timer := logging.Start("waiting for the deployment to finish",
		"service", name, "rollingBack", rollingBack, "timeout", finishTimeout)

	for {
		dep, err := d.describeDeployment(ctx, deployment)
		if err != nil {
			slog.Debug("could not read the service deployment", "service", name, "err", err)
		} else if dep != nil {
			switch dep.Status {
			case ecstypes.ServiceDeploymentStatusSuccessful:
				if rollingBack {
					return fmt.Errorf(
						"%s: asked ECS to roll back and it reported success instead", name)
				}
				timer.Done()
				return nil
			case ecstypes.ServiceDeploymentStatusRollbackSuccessful:
				if rollingBack {
					timer.Done()
					return nil
				}
				return fmt.Errorf("%s: ECS rolled the deployment back: %s",
					name, orNone(awssdk.ToString(dep.StatusReason)))
			case ecstypes.ServiceDeploymentStatusRollbackFailed,
				ecstypes.ServiceDeploymentStatusStopped:
				return fmt.Errorf("%s: the deployment ended as %s: %s", name,
					dep.Status, orNone(awssdk.ToString(dep.StatusReason)))
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%s: the deployment had not finished within %s",
				name, finishTimeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Settle has nothing to do here, and that is the point.
//
// Everywhere else the tool maintains the invariant by hand: one revision
// serving, everything else switched off. On ECS the CLEAN_UP stage is the
// platform's own, and it terminates the old side once the bake time is up. So
// there is nothing left running that this could switch off, and nothing this
// could switch off that ECS would not bring back.
func (d *Driver) Settle(context.Context, *target.Change) error { return nil }

// Tidy has nothing to do here either, for the same reason as Settle.
func (d *Driver) Tidy(context.Context, *config.Target) error { return nil }

// Point refuses, because on ECS there is no side to point at *by name*.
//
// Everywhere else a label is a name in a traffic block and moving it is one
// write. Here the target groups are the platform's and they swap on every
// deployment, so a label is a role in one release rather than an address.
//
// That is not the same as saying there is no way back — see Undo, which is what
// `evolve-deploy rollback` uses here.
func (d *Driver) Point(_ context.Context, t *config.Target, label string) error {
	return fmt.Errorf(
		"ecs service %s has no side called %q to point at.\n"+
			"    ECS owns both target groups and swaps them itself, so a side is a role in\n"+
			"    one release rather than something standing that can be named.\n"+
			"    Use `evolve-deploy rollback` instead: while the previous version is still\n"+
			"    running it puts the traffic back, and it says so plainly when that window\n"+
			"    has closed.",
		t.Name, label)
}

// Undo reverses a release ECS is still running, which is the ECS rollback.
//
// There is no standing side here to point at, but for as long as the deployment
// has not finished, the previous version is still up and ECS has a call that
// puts the traffic back on it. That covers two windows worth having:
//
//   - the bake time after the switch, which is exactly what bake_time buys and
//     what made it worth a config field at all;
//   - a release whose pipeline died while paused at the smoke gate, which
//     nothing else would ever clean up.
//
// Once the deployment has finished there is nothing to reverse: CLEAN_UP has
// terminated the old tasks and going back is a deploy. errNoWindow says so, and
// the command turns it into that advice rather than a failure.
func (d *Driver) Undo(ctx context.Context, t *config.Target) (string, error) {
	dep, err := d.reversibleDeployment(ctx, t)
	if err != nil {
		return "", err
	}
	if dep == nil {
		return "", target.ErrNoWindow
	}

	arn := awssdk.ToString(dep.ServiceDeploymentArn)
	stage := orNone(string(dep.LifecycleStage))

	timer := logging.Start("stopping the deployment", "service", t.Name,
		"deployment", arn, "stage", stage)
	if _, err := d.ecs.StopServiceDeployment(ctx, &ecs.StopServiceDeploymentInput{
		ServiceDeploymentArn: awssdk.String(arn),
		StopType:             ecstypes.StopServiceDeploymentStopTypeRollback,
	}); err != nil {
		return "", fmt.Errorf("%s: rolling the deployment back: %w", t.Name, err)
	}
	timer.Done()

	if err := d.awaitFinish(ctx, t.Name, arn, true); err != nil {
		return "", err
	}
	return "rolled back from " + stage, nil
}

// reversibleDeployment finds a deployment that has not finished, and is
// therefore one ECS still has the previous version standing behind.
//
// A rollback already under way is not one of them: asking again would be asking
// for something that is happening.
func (d *Driver) reversibleDeployment(
	ctx context.Context,
	t *config.Target,
) (*ecstypes.ServiceDeployment, error) {
	return d.inFlight(ctx, t, []ecstypes.ServiceDeploymentStatus{
		ecstypes.ServiceDeploymentStatusPending,
		ecstypes.ServiceDeploymentStatusInProgress,
	})
}

// Traffic reports which version is serving. There is no split to show — ECS
// shifts all at once, as this tool does everywhere — so this is one row.
func (d *Driver) Traffic(ctx context.Context, t *config.Target) ([]target.TrafficEntry, error) {
	sides, err := d.Sides(ctx, t)
	if err != nil {
		return nil, err
	}
	return []target.TrafficEntry{{
		Label:    sides.Active.Label,
		Revision: sides.Active.Revision,
		Version:  sides.Active.Version,
		Weight:   100,
	}}, nil
}

// --- reads -------------------------------------------------------------------

func (d *Driver) describeService(ctx context.Context, t *config.Target) (*ecstypes.Service, error) {
	out, err := d.ecs.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster:  awssdk.String(t.Cluster),
		Services: []string{t.Name},
	})
	if err != nil {
		return nil, fmt.Errorf("ecs service %s: %w", t.Name, err)
	}
	if len(out.Services) == 0 {
		return nil, fmt.Errorf("ecs service %s not found in cluster %s", t.Name, t.Cluster)
	}
	return &out.Services[0], nil
}

// latestDeployment finds the deployment this release started.
//
// Filtered to the ones that are not finished: a service that has been deployed
// before has a list of them, and the only interesting one is the deployment
// still running.
func (d *Driver) latestDeployment(
	ctx context.Context,
	t *config.Target,
) (*ecstypes.ServiceDeployment, error) {
	// A rollback ECS started by itself is included here, because Stage has to
	// notice that and stop rather than wait out its timeout at a gate that will
	// never come.
	return d.inFlight(ctx, t, []ecstypes.ServiceDeploymentStatus{
		ecstypes.ServiceDeploymentStatusPending,
		ecstypes.ServiceDeploymentStatusInProgress,
		ecstypes.ServiceDeploymentStatusRollbackRequested,
		ecstypes.ServiceDeploymentStatusRollbackInProgress,
	})
}

// inFlight returns the newest deployment in one of the given states, or nil.
func (d *Driver) inFlight(
	ctx context.Context,
	t *config.Target,
	status []ecstypes.ServiceDeploymentStatus,
) (*ecstypes.ServiceDeployment, error) {
	list, err := d.ecs.ListServiceDeployments(ctx, &ecs.ListServiceDeploymentsInput{
		Cluster:    awssdk.String(t.Cluster),
		Service:    awssdk.String(t.Name),
		Status:     status,
		MaxResults: awssdk.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("listing deployments of %s: %w", t.Name, err)
	}
	if len(list.ServiceDeployments) == 0 {
		return nil, nil
	}
	return d.describeDeployment(ctx,
		awssdk.ToString(list.ServiceDeployments[0].ServiceDeploymentArn))
}

func (d *Driver) describeDeployment(
	ctx context.Context,
	arn string,
) (*ecstypes.ServiceDeployment, error) {
	if arn == "" {
		return nil, nil
	}
	out, err := d.ecs.DescribeServiceDeployments(ctx, &ecs.DescribeServiceDeploymentsInput{
		ServiceDeploymentArns: []string{arn},
	})
	if err != nil {
		return nil, fmt.Errorf("describing deployment %s: %w", arn, err)
	}
	if len(out.ServiceDeployments) == 0 {
		return nil, nil
	}
	return &out.ServiceDeployments[0], nil
}

// planECSBlueGreen builds the next task definition for a service released a
// side at a time.
//
// Almost all of it is planECS: ECS keeps the shape in an immutable task
// definition either way, and the difference between direct and blue-green is
// who moves the traffic afterwards, not what gets registered. What is different
// is the two refusals at the top, and both are about ECS not having the thing
// the other clouds have.
func (d *Driver) planECSBlueGreen(
	ctx context.Context,
	want *target.Desired,
) (*target.Change, error) {
	t := want.Target

	if t.TestURL == "" {
		return nil, fmt.Errorf(
			"ecs service %s: blue-green needs `test_url`.\n"+
				"    The staged side is reached through the test listener rule, and a rule is\n"+
				"    not an address — the tool cannot derive one from it. Write down where\n"+
				"    that rule answers:\n"+
				"      test_url: https://%s-test.internal.example", t.Name, t.Name)
	}
	if len(t.Strategy.Env) > 0 {
		return nil, fmt.Errorf(
			"ecs service %s: `strategy.env` cannot work here.\n"+
				"    Per-side environment exists so a staged side reaches its own downstream —\n"+
				"    green calling green — and that needs the two sides to alternate and keep\n"+
				"    their names. ECS owns both target groups and swaps them itself, so the\n"+
				"    sides are roles in one release rather than two standing environments.",
			t.Name)
	}
	if t.Strategy.KeepsWarm() {
		return nil, fmt.Errorf(
			"ecs service %s: `keep_warm` cannot be honoured here.\n"+
				"    ECS terminates the old side itself at CLEAN_UP, so it is never a standing\n"+
				"    cost and never a standing rollback. Use `bake_time` for the window before\n"+
				"    that happens.", t.Name)
	}

	sides, err := d.Sides(ctx, t)
	if err != nil {
		return nil, err
	}

	ch, err := d.planECS(ctx, want)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		// Nothing changed, but the side still needs a deployment of its own:
		// the staged side is only a stack if every service is on it. Whether
		// this is deployed is the release's call — see Change.Carry.
		ch = &target.Change{
			Service:     want.Service,
			Target:      t,
			FromVersion: sides.Active.Version,
			ToVersion:   want.Version,
			Carry:       true,
		}
		base, err := d.describeTaskDef(ctx, t.Base)
		if err != nil {
			return nil, fmt.Errorf("base task definition %q: %w", t.Base, err)
		}
		name, err := target.PickContainer(
			containerNames(base.ContainerDefinitions), t.Container, ecsAppContainer)
		if err != nil {
			return nil, fmt.Errorf("base task definition %s: %w", t.Base, err)
		}
		img, err := image.Retag(awssdk.ToString(findContainer(
			base.ContainerDefinitions, name).Image), want.Version)
		if err != nil {
			return nil, err
		}
		ch.Payload = &ecsPayload{
			register: renderTaskDef(base, t.Name, name, img, want.Env, want.ManageEnv),
			previous: sides.Active.Revision,
		}
	}

	direct := ch.Payload.(*ecsPayload)
	ch.Sides = sides
	ch.Payload = &bgPayload{register: direct.register, previous: direct.previous}
	return ch, nil
}
