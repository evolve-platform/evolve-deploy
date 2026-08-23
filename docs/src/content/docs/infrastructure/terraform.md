---
title: What Terraform must do
description: The one lifecycle rule that stops Terraform rolling the image back, the ECS two-family pattern, and what blue-green needs declared.
sidebar:
  order: 1
---

Terraform owns the infrastructure; this tool owns the version. That boundary
needs a small number of things declared on your side, and the tool reads them
and refuses when one is missing rather than writing it.

## 1. Let go of the image

Otherwise the next `terraform apply` rolls the image back to whatever the module
declared. One line per resource:

```hcl
# azurerm_container_app
lifecycle { ignore_changes = [template[0].container[0].image] }

# google_cloud_run_v2_service
lifecycle { ignore_changes = [template[0].containers[0].image] }

# aws_lambda_function
lifecycle { ignore_changes = [s3_key] }
```

Add the environment to that list once a service's [`env`](../../configuration/environment/)
moves into the deploy config — and stop declaring it here at all. The config is
the whole environment from that point, so the first release replaces whatever
Terraform wrote; a declaration left behind is not a fallback, it is a second
answer to a question that now has one. Anything that used to live here and is not
in the deploy config belongs in a parameter store the service reads for itself.

```hcl
lifecycle {
  ignore_changes = [
    template[0].container[0].image,
    template[0].container[0].env,
  ]
}
```

Azure and GCP need nothing else. Updates carry only the template — a merge patch
on Azure, a `template` field mask on GCP — so ingress, identity, IAM, traffic
split and declared secrets are untouched.

:::note[On Azure that is necessity, not tidiness]
A full write-back would blank every secret, because a read never returns their
values.
:::

## 2. ECS: two task definition families

ECS is the exception to everything above. Image, environment, cpu and probes all
live in one immutable `container_definitions` blob, so field-level
`ignore_changes` is impossible.

Instead each owner gets its own task definition family:

- **Terraform registers the shape into `<name>-base`.** Nothing points at it.
- **`evolve-deploy` derives the running family from it** and registers that.

```hcl
resource "aws_ecs_task_definition" "purchase_base" {
  family = "purchase-base"       # the tool's default is <name>-base
  # cpu, memory, probes, log config, sidecars — everything but the version
  container_definitions = jsonencode([...])
}

resource "aws_ecs_service" "purchase" {
  name            = "purchase"
  cluster         = aws_ecs_cluster.platform.id
  task_definition = aws_ecs_task_definition.purchase_base.arn   # bootstrap only
  lifecycle { ignore_changes = [task_definition] }
}
```

A memory change in Terraform then lands on the next deploy, and Terraform can
never roll the image back.

The same goes for the environment as everywhere else: once a service declares
[`env`](../../configuration/environment/), take those variables out of the
released container's definition in the base. There is no `ignore_changes` to add
here — the tool simply replaces that container's `environment` and `secrets` with
what the config declares, so anything left in the base is a second answer that
never wins. Everything else in the base is still read and carried through: cpu,
memory, probes, log configuration, and the sidecars with their own environments.

Override the family name with `base:` on the service or target if `<name>-base`
is not your convention.

## 3. Blue-green: declare the traffic

The tool reads what you declared and refuses when it is not there. It never
creates it.

### Azure Container Apps

Set `revision_mode = "Multiple"` and bootstrap the traffic block, then let go of
it:

```hcl
resource "azurerm_container_app" "site" {
  revision_mode = "Multiple"

  ingress {
    traffic_weight {
      label           = "blue"
      percentage      = 100
      latest_revision = true
    }
  }

  lifecycle {
    ignore_changes = [
      template[0].container[0].image,
      ingress[0].traffic_weight,     # the tool owns this from now on
    ]
  }
}
```

### GCP Cloud Run

No revision mode to switch on — several revisions with a split are always
allowed. Bootstrap the traffic block with a tag on the side that serves:

```hcl
resource "google_cloud_run_v2_service" "site" {
  traffic {
    type     = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    tag      = "blue"
    percent  = 100
  }

  lifecycle {
    ignore_changes = [
      template[0].containers[0].image,
      traffic,
    ]
  }
}
```

`keep_warm` is refused on Cloud Run: keeping a revision warm is
`scaling.min_instance_count` on the template, which is yours.

### AWS ECS

ECS owns its own blue/green engine, so what it needs declared is more:
an alternate target group, a production listener rule, a test listener rule and
a role, all on the service, plus `deployment_controller { type = "ECS" }`.

```hcl
resource "aws_ecs_service" "site" {
  deployment_controller { type = "ECS" }
  # advanced_configuration: alternate_target_group_arn,
  # production_listener_rule, test_listener_rule, role_arn
}
```

The tool reads them off the service and refuses when one is missing — it never
writes them. And because a listener rule is not an address, the target needs
[`test_url`](../../blue-green/clouds/#test_url-is-required) written down.

## 4. Declare the secrets you will reference

On Azure a secret must be declared *on the resource* — Key Vault URL and the
identity allowed to read it — and then referred to by name. A
`${secret:ctp-secret}` naming something Terraform never declared **fails the
plan** rather than producing a revision that cannot start:

```hcl
resource "azurerm_container_app" "purchase" {
  secret {
    name                = "ctp-client-secret"
    key_vault_secret_id = azurerm_key_vault_secret.ctp.versionless_id
    identity            = azurerm_user_assigned_identity.purchase.id
  }
}
```

See [References and secrets](../../configuration/references/) for what
`${secret:}` names on each cloud — it differs, and it has to.

## 5. `envFrom`, if you use it

`envFrom` expands a JSON object out of a *parameter* store — never a secret
store, so bulk expansion can never mean reading a secret. Terraform writes it
with `jsonencode`:

```hcl
resource "azurerm_app_configuration_key" "discover_setup" {
  configuration_store_id = azurerm_app_configuration.main.id
  key                    = "/evolve/${var.env}/discover/setup"
  value                  = jsonencode(local.discover_env)
  content_type           = "application/json"
}
```

## What stays entirely yours

Cpu, memory, probes, scaling, networking, IAM, load balancers, queues and event
source mappings. The tool's contract is one sentence:

> I set the image and the environment on the running resource, and leave
> everything else alone.
