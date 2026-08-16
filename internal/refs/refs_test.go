package refs

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Value
		wantErr bool
	}{
		{
			name: "literal",
			in:   "info",
			want: Value{Kind: Literal, Literal: "info", Raw: "info"},
		},
		{
			name: "literal that looks like a url",
			in:   "https://api.example.com/graphql",
			want: Value{Kind: Literal, Literal: "https://api.example.com/graphql", Raw: "https://api.example.com/graphql"},
		},
		{
			name: "param",
			in:   "${param:/platform/redis-url}",
			want: Value{Kind: Param, Name: "/platform/redis-url", Raw: "${param:/platform/redis-url}"},
		},
		{
			name: "secret",
			in:   "${secret:purchase/ctp-client-secret}",
			want: Value{Kind: Secret, Name: "purchase/ctp-client-secret", Raw: "${secret:purchase/ctp-client-secret}"},
		},
		{
			name: "arn as a secret name keeps its colons",
			in:   "${secret:arn:aws:secretsmanager:eu-west-1:1234:secret:x}",
			want: Value{Kind: Secret, Name: "arn:aws:secretsmanager:eu-west-1:1234:secret:x",
				Raw: "${secret:arn:aws:secretsmanager:eu-west-1:1234:secret:x}"},
		},
		{name: "partial interpolation is rejected", in: "https://${param:host}/x", wantErr: true},
		{name: "unknown scheme", in: "${env:HOME}", wantErr: true},
		{name: "no scheme", in: "${whatever}", wantErr: true},
		{name: "empty name", in: "${param:}", wantErr: true},
		{name: "unexpanded env leaves a nested ref", in: "${param:/a/${env}/b}", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %+v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Parse(%q)\n got %+v\nwant %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSubstituteRunsBeforeParse(t *testing.T) {
	// The whole point of substituting first: a reference may contain ${env},
	// and after expansion there is exactly one ${…} left.
	got, err := Parse(Substitute("${param:/evolve/${env}/purchase/setup}", "tst"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Kind != Param || got.Name != "/evolve/tst/purchase/setup" {
		t.Errorf("got %+v, want a param named /evolve/tst/purchase/setup", got)
	}
}

func TestParseRefRejectsLiterals(t *testing.T) {
	if _, err := ParseRef("just-a-string"); err == nil {
		t.Error("ParseRef accepted a literal; envFrom needs a reference")
	}
	if _, err := ParseRef("${param:/x}"); err != nil {
		t.Errorf("ParseRef rejected a valid reference: %v", err)
	}
}
