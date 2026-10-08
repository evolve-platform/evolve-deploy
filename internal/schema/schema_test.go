package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func check(t *testing.T, body string) []string {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return Check(&doc)
}

const awsHeader = `cloud:
  provider: aws
  account: "513712104672"
  region: eu-west-1
`

func TestEveryMistakeIsNamedWhereItIs(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "a misspelt key suggests the one that exists",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    clsuter: c\n",
			want: "services.site.clsuter: line 9: unknown field (did you mean cluster?)",
		},
		{
			name: "spelling is compared without case or separators",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    dependsOn: [x]\n",
			want: "services.site.dependsOn: line 9: unknown field (did you mean depends_on?)",
		},
		{
			name: "a key nothing resembles lists what would have been right",
			body: awsHeader + "refs:\n  whatever: x\nservices:\n  site: {version: a, type: ecs}\n",
			want: "refs.whatever: line 6: unknown field (want one of resolve)",
		},
		{
			name: "one name where a list goes says how to write a list of one",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    depends_on: purchase\n",
			want: `services.site.depends_on: line 9: want a list, got "purchase"; a list of one is written [purchase]`,
		},
		{
			name: "a field from another provider says which provider it is",
			body: "cloud: {provider: aws, account: '1', region: r, project: p}\nservices:\n  site: {version: a, type: ecs}\n",
			want: "cloud.project: line 1: does not apply when provider is aws",
		},
		{
			name: "missing addressing says which provider needs it",
			body: "cloud: {provider: gcp, region: r}\nservices:\n  site: {version: a, type: cloud-run}\n",
			want: "cloud.project: line 1: required when provider is gcp",
		},
		{
			name: "a target type from another cloud",
			body: awsHeader + "services:\n  site:\n    version: a\n    targets:\n      - {type: cloud-run, name: x}\n",
			want: `services.site.targets[0].type: line 9: "cloud-run" is not valid when cloud.provider is aws (want one of ecs, lambda)`,
		},
		{
			name: "a field that belongs to another target type",
			body: awsHeader + "services:\n  ev:\n    version: a\n    targets:\n      - {type: lambda, cluster: c}\n",
			want: "services.ev.targets[0].cluster: line 9: does not apply when type is lambda",
		},
		{
			name: "nested inside a field that does apply",
			body: "cloud: {provider: azure, subscription: s, resource_group: g}\nservices:\n  fn:\n    version: a\n    targets:\n      - {type: function-app, code: {bucket: b}}\n",
			want: "services.fn.targets[0].code.bucket: line 6: does not apply when type is function-app",
		},
		{
			name: "an unknown action suggests the one meant",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    after:\n      - {uses: honycomb}\n",
			want: `services.site.after[0].uses: line 10: "honycomb" is not one of honeycomb, http, job, sentry (did you mean honeycomb?)`,
		},
		{
			name: "an action's options are called options",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    after:\n      - {uses: honeycomb, with: {dataset: d, mesage: m}}\n",
			want: "services.site.after[0].with.mesage: line 10: unknown option (did you mean message?)",
		},
		{
			name: "an action with nothing to go on",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    after:\n      - uses: http\n",
			want: "services.site.after[0].with: line 10: required when uses is http",
		},
		{
			name: "a hook that is neither form",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    after:\n      - [echo, hi]\n",
			want: "services.site.after[0]: line 10: a hook is either a command line or a block with `cmd` or `uses`, not a list",
		},
		{
			name: "a number where a word was written",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    after:\n      - {uses: http, with: {url: u, status: ok}}\n",
			want: `services.site.after[0].with.status: line 10: want a whole number, got "ok"`,
		},
		{
			name: "a key written twice",
			body: awsHeader + "services:\n  site:\n    version: a\n    type: ecs\n    version: b\n",
			want: "services.site.version: line 9: already set on line 7",
		},
		{
			name: "a missing top-level block",
			body: awsHeader,
			want: "services: line 1: required",
		},
		{
			name: "an empty services block",
			body: awsHeader + "services: {}\n",
			want: "services: line 5: needs at least one entry",
		},
		{
			name: "a file that is not a block at all",
			body: "- a\n- b\n",
			want: "line 1: want a block, got a list",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msgs := check(t, tc.body)
			if !slices.Contains(msgs, tc.want) {
				t.Errorf("got:\n  %s\nwant among them:\n  %s", strings.Join(msgs, "\n  "), tc.want)
			}
		})
	}
}

// What the decoder has always taken has to go on passing: a file that
// deployed yesterday and is refused today is a broken pipeline, not a
// stricter tool.
func TestWhatTheDecoderTakesStillPasses(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "numbers and booleans in string fields",
			body: awsHeader + "services:\n  site:\n    version: 1234567\n    type: ecs\n    env:\n      PORT: 8080\n      DEBUG: true\n      RATIO: 1.10\n",
		},
		{
			name: "a date where text goes",
			body: awsHeader + "services:\n  site:\n    version: 2024-01-01\n    type: ecs\n",
		},
		{
			name: "an empty key is a key left out",
			body: awsHeader + "strategy:\nservices:\n  site:\n    version: a\n    type: ecs\n    after:\n      # - echo later\n",
		},
		{
			name: "a command line YAML reads as a boolean",
			body: awsHeader + "strategy:\n  type: blue-green\n  smoke: [ true ]\nservices:\n  site: {version: a, type: ecs}\n",
		},
		{
			name: "anchors and merge keys",
			body: awsHeader + `services:
  site: &ecs
    version: a
    type: ecs
    cluster: platform
  admin:
    <<: *ecs
    version: b
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if msgs := check(t, tc.body); len(msgs) > 0 {
				t.Errorf("refused:\n  %s", strings.Join(msgs, "\n  "))
			}
		})
	}
}

// A merged key that the mapping also sets is the mapping's, so only the
// merged ones that are wrong are reported.
func TestAMergedKeyIsOverriddenNotDuplicated(t *testing.T) {
	msgs := check(t, awsHeader+`base: &base
  version: a
services:
  site:
    <<: *base
    version: b
    type: ecs
`)
	for _, m := range msgs {
		if strings.Contains(m, "already set") {
			t.Errorf("a merged key was reported as set twice: %s", m)
		}
	}
}

func TestTheExamplesPass(t *testing.T) {
	paths, err := filepath.Glob("../../examples/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if msgs := check(t, string(body)); len(msgs) > 0 {
				t.Errorf("refused:\n  %s", strings.Join(msgs, "\n  "))
			}
		})
	}
}

func TestDocumentPointsAtItsRelease(t *testing.T) {
	var doc struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(Document(URL("0.13.0")), &doc); err != nil {
		t.Fatal(err)
	}
	if want := "https://deploy.evolve-platform.com/schema/v0.13.0.json"; doc.ID != want {
		t.Errorf("$id = %q, want %q", doc.ID, want)
	}
	if got := URL("dev"); got != LatestURL {
		t.Errorf("a dev build points at %q, want the latest", got)
	}
}
