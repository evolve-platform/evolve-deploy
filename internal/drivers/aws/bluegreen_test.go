package aws

import (
	"strings"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/evolve-platform/evolve-deploy/internal/config"
)

func bgService() *ecstypes.Service {
	return &ecstypes.Service{
		DeploymentController: &ecstypes.DeploymentController{
			Type: ecstypes.DeploymentControllerTypeEcs,
		},
		LoadBalancers: []ecstypes.LoadBalancer{{
			AdvancedConfiguration: &ecstypes.AdvancedConfiguration{
				AlternateTargetGroupArn: awssdk.String("arn:tg:alternate"),
				ProductionListenerRule:  awssdk.String("arn:rule:prod"),
				TestListenerRule:        awssdk.String("arn:rule:test"),
				RoleArn:                 awssdk.String("arn:role:ecs"),
			},
		}},
	}
}

func ecsTarget() *config.Target {
	return &config.Target{
		Type: config.TypeECS, Name: "site", Cluster: "platform",
		Strategy: &config.Strategy{
			Type: config.StrategyBlueGreen, Labels: config.DefaultLabels,
		},
	}
}

// Routing on ECS is real infrastructure and the tool reads it rather than
// writing it. Every one of these has to exist before a release can stage, so
// finding out at the switch would be finding out far too late.
func TestCheckBlueGreenService(t *testing.T) {
	if err := checkBlueGreenService(ecsTarget(), bgService()); err != nil {
		t.Fatalf("a fully configured service was refused: %v", err)
	}

	cases := []struct {
		name    string
		mangle  func(*ecstypes.Service)
		wantErr string
	}{{
		name: "the CODE_DEPLOY controller is a different mechanism",
		mangle: func(s *ecstypes.Service) {
			s.DeploymentController.Type = ecstypes.DeploymentControllerTypeCodeDeploy
		},
		wantErr: "blue-green needs ECS",
	}, {
		name:    "no controller at all",
		mangle:  func(s *ecstypes.Service) { s.DeploymentController = nil },
		wantErr: "blue-green needs ECS",
	}, {
		name:    "no load balancer to shift traffic between",
		mangle:  func(s *ecstypes.Service) { s.LoadBalancers = nil },
		wantErr: "nothing to shift traffic between",
	}, {
		name: "no alternate target group",
		mangle: func(s *ecstypes.Service) {
			s.LoadBalancers[0].AdvancedConfiguration.AlternateTargetGroupArn = nil
		},
		wantErr: "alternate_target_group_arn",
	}, {
		// Without this there is no address for a smoke test, which is the
		// entire reason to stage anything.
		name: "no test listener rule",
		mangle: func(s *ecstypes.Service) {
			s.LoadBalancers[0].AdvancedConfiguration.TestListenerRule = nil
		},
		wantErr: "test_listener_rule",
	}, {
		name: "several missing are reported together",
		mangle: func(s *ecstypes.Service) {
			adv := s.LoadBalancers[0].AdvancedConfiguration
			adv.RoleArn = nil
			adv.ProductionListenerRule = nil
		},
		wantErr: "production_listener_rule, role_arn",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := bgService()
			c.mangle(svc)

			err := checkBlueGreenService(ecsTarget(), svc)
			if err == nil {
				t.Fatalf("no error, want one mentioning %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// The gate is one specific hook at one specific stage. Answering anything else
// would continue a deployment that has not reached the point where a smoke test
// means something.
func TestPendingHook(t *testing.T) {
	waiting := ecstypes.ServiceDeployment{
		LifecycleStage: ecstypes.ServiceDeploymentLifecycleStagePostTestTrafficShift,
		LifecycleHookDetails: []ecstypes.DeploymentLifecycleHookDetail{{
			HookId:     awssdk.String("hook-1"),
			Status:     ecstypes.DeploymentLifecycleHookStatusAwaitingAction,
			TargetType: ecstypes.DeploymentLifecycleHookTargetTypePause,
		}},
	}
	if got := pendingHook(&waiting); got != "hook-1" {
		t.Errorf("hook = %q, want hook-1", got)
	}

	// Test traffic has not fully shifted yet, so there is nothing to test
	// against even though a hook exists.
	early := waiting
	early.LifecycleStage = ecstypes.ServiceDeploymentLifecycleStagePostScaleUp
	if got := pendingHook(&early); got != "" {
		t.Errorf("hook = %q at %s, want none", got, early.LifecycleStage)
	}

	// A hook that is not waiting is not a gate.
	running := ecstypes.ServiceDeployment{
		LifecycleStage: ecstypes.ServiceDeploymentLifecycleStagePostTestTrafficShift,
		LifecycleHookDetails: []ecstypes.DeploymentLifecycleHookDetail{{
			HookId:     awssdk.String("hook-1"),
			Status:     ecstypes.DeploymentLifecycleHookStatusInProgress,
			TargetType: ecstypes.DeploymentLifecycleHookTargetTypePause,
		}},
	}
	if got := pendingHook(&running); got != "" {
		t.Errorf("hook = %q while in progress, want none", got)
	}

	// A Lambda hook is somebody else's, and answering it is not ours to do.
	lambda := waiting
	lambda.LifecycleHookDetails = []ecstypes.DeploymentLifecycleHookDetail{{
		HookId:     awssdk.String("hook-2"),
		Status:     ecstypes.DeploymentLifecycleHookStatusAwaitingAction,
		TargetType: ecstypes.DeploymentLifecycleHookTargetTypeAwsLambda,
	}}
	if got := pendingHook(&lambda); got != "" {
		t.Errorf("hook = %q for a Lambda hook, want none", got)
	}
}

// ECS carries traffic, so a release moves it — but it owns the sides and takes
// the old one away, so nothing outside a release can point at one.
func TestECSIsRoutableButNotPointable(t *testing.T) {
	var d Driver
	if !d.Routable(config.TypeECS) {
		t.Error("a release does move an ECS service's traffic")
	}
	if d.Pointable(config.TypeECS) {
		t.Error("there is no standing side on ECS to point at")
	}
	if d.Routable(config.TypeLambda) {
		t.Error("a lambda has no listener rule; it rides along")
	}
}

// The two success states are checked against what was asked for, not accepted
// on their own. A deployment that reports SUCCESSFUL after a rollback was
// requested has not done what it was told, and reading that as fine would leave
// the wrong version serving with a green line above it.
func TestWantRollback(t *testing.T) {
	if !wantRollback(ecstypes.DeploymentLifecycleHookActionRollback) {
		t.Error("ROLLBACK asks for a rollback")
	}
	if wantRollback(ecstypes.DeploymentLifecycleHookActionContinue) {
		t.Error("CONTINUE does not")
	}
}
