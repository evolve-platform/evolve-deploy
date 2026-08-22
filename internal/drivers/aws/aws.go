// Package aws implements the ECS and Lambda targets.
package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/evolve-platform/evolve-deploy/internal/config"
	"github.com/evolve-platform/evolve-deploy/internal/refs"
	"github.com/evolve-platform/evolve-deploy/internal/target"
)

// Driver is the AWS implementation of target.Driver.
type Driver struct {
	file *config.File

	ecs    *ecs.Client
	ecr    *ecr.Client
	lambda *lambda.Client
	ssm    *ssm.Client
	secret *secretsmanager.Client
	sts    *sts.Client
	s3     *s3.Client
}

// New builds a driver from the ambient credential chain. The config file names
// the account and region; the credentials must agree, which Verify checks.
func New(ctx context.Context, f *config.File) (*Driver, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(f.Cloud.Region))
	if err != nil {
		return nil, fmt.Errorf("aws: %w", err)
	}
	return &Driver{
		file:   f,
		ecs:    ecs.NewFromConfig(cfg),
		ecr:    ecr.NewFromConfig(cfg),
		lambda: lambda.NewFromConfig(cfg),
		ssm:    ssm.NewFromConfig(cfg),
		secret: secretsmanager.NewFromConfig(cfg),
		sts:    sts.NewFromConfig(cfg),
		s3:     s3.NewFromConfig(cfg),
	}, nil
}

func (d *Driver) Name() string { return "aws" }

// Capabilities: ECS resolves both parameter and secret references itself
// through the task definition's `secrets` list, which accepts SSM parameter and
// Secrets Manager ARNs alike. Lambda resolves nothing — its environment
// variables are literal strings — so anything referenced there is read by the
// tool and written out.
func (d *Driver) Capabilities(t config.TargetType) target.Capability {
	switch t {
	case config.TypeECS:
		return target.Capability{NativeParam: true, NativeSecret: true}
	default:
		return target.Capability{}
	}
}

// Verify refuses to run against the wrong account.
//
// This is the one guard that earns its keep: on AWS the account is implicit in
// the credentials, so everything else in the config file is reviewable except
// where it points. One wrong role ARN in a workflow and a test release lands on
// production.
func (d *Driver) Verify(ctx context.Context) error {
	out, err := d.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("aws: cannot determine the active account: %w", err)
	}
	if awssdk.ToString(out.Account) != d.file.Cloud.Account {
		return fmt.Errorf(
			"aws: %s targets account %s but the active credentials are for %s",
			d.file.Path, d.file.Cloud.Account, awssdk.ToString(out.Account))
	}
	return nil
}

func (d *Driver) Resolver() refs.Resolver { return &resolver{d: d} }

func (d *Driver) Plan(ctx context.Context, want *target.Desired) (*target.Change, error) {
	switch want.Target.Type {
	case config.TypeECS:
		if want.Target.Strategy.IsBlueGreen() {
			return d.planECSBlueGreen(ctx, want)
		}
		return d.planECS(ctx, want)
	case config.TypeLambda:
		return d.planLambda(ctx, want)
	default:
		return nil, &target.ErrNotImplemented{Cloud: "aws", Type: want.Target.Type}
	}
}

func (d *Driver) Apply(ctx context.Context, ch *target.Change) error {
	switch ch.Target.Type {
	case config.TypeECS:
		return d.applyECS(ctx, ch)
	case config.TypeLambda:
		return d.applyLambda(ctx, ch)
	default:
		return &target.ErrNotImplemented{Cloud: "aws", Type: ch.Target.Type}
	}
}

func (d *Driver) Revert(ctx context.Context, ch *target.Change) error {
	switch ch.Target.Type {
	case config.TypeECS:
		return d.revertECS(ctx, ch)
	case config.TypeLambda:
		return d.revertLambda(ctx, ch)
	default:
		return &target.ErrNotImplemented{Cloud: "aws", Type: ch.Target.Type}
	}
}

// resolver reads SSM Parameter Store and Secrets Manager.
type resolver struct{ d *Driver }

// Verify confirms a reference exists without reading its value where the API
// allows it.
//
// Secrets Manager has DescribeSecret, which returns metadata only and needs a
// lighter permission than GetSecretValue. SSM has no equivalent: GetParameter
// returns the value, and DescribeParameters with a name filter is clumsy and
// rate-limited. So a parameter is read and discarded — acceptable because
// parameters hold configuration, and anything sensitive belongs in a secret.
func (r *resolver) Verify(ctx context.Context, v refs.Value) error {
	switch v.Kind {
	case refs.Secret:
		_, err := r.d.secret.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{
			SecretId: awssdk.String(v.Name),
		})
		if err != nil {
			var notFound *smtypes.ResourceNotFoundException
			if errors.As(err, &notFound) {
				return fmt.Errorf("secret %q does not exist", v.Name)
			}
			return fmt.Errorf("secret %q: %w", v.Name, err)
		}
		return nil
	case refs.Param:
		_, err := r.readParam(ctx, v.Name)
		return err
	default:
		return nil
	}
}

func (r *resolver) Read(ctx context.Context, v refs.Value) (string, error) {
	switch v.Kind {
	case refs.Secret:
		out, err := r.d.secret.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
			SecretId: awssdk.String(v.Name),
		})
		if err != nil {
			var notFound *smtypes.ResourceNotFoundException
			if errors.As(err, &notFound) {
				return "", fmt.Errorf("secret %q does not exist", v.Name)
			}
			return "", fmt.Errorf("secret %q: %w", v.Name, err)
		}
		if out.SecretString == nil {
			return "", fmt.Errorf("secret %q holds binary data, which cannot be an environment variable", v.Name)
		}
		return awssdk.ToString(out.SecretString), nil
	case refs.Param:
		return r.readParam(ctx, v.Name)
	default:
		return v.Literal, nil
	}
}

// ReadMap expands a bulk reference. The stored value is a JSON object, which is
// what Terraform writes with jsonencode(local.env_vars).
func (r *resolver) ReadMap(ctx context.Context, v refs.Value) (map[string]string, error) {
	raw, err := r.Read(ctx, v)
	if err != nil {
		return nil, err
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%s does not hold a JSON object of strings: %w", v.Raw, err)
	}
	return out, nil
}

func (r *resolver) readParam(ctx context.Context, name string) (string, error) {
	out, err := r.d.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           awssdk.String(name),
		WithDecryption: awssdk.Bool(true),
	})
	if err != nil {
		var notFound *ssmtypes.ParameterNotFound
		if errors.As(err, &notFound) {
			return "", fmt.Errorf("parameter %q does not exist", name)
		}
		return "", fmt.Errorf("parameter %q: %w", name, err)
	}
	return awssdk.ToString(out.Parameter.Value), nil
}
