package evaluator

import "testing"

func TestHarborRewardValidityAndScores(t *testing.T) {
	for _, tt := range []struct {
		name, data, verdict string
		score               *float64
		bad                 bool
	}{
		{"success", `{"reward":1,"valid":1}`, "success", number(1), false},
		{"zero", `{"reward":0,"valid":1}`, "test_failure", number(0), false},
		{"partial", `{"reward":0.5,"valid":1}`, "test_failure", number(.5), false},
		{"infra", `{"reward":0,"valid":0}`, "infra_error", nil, true},
		{"missing valid", `{"reward":0}`, "infra_error", nil, true},
		{"out of range", `{"reward":1.1,"valid":1}`, "infra_error", nil, true},
		{"bool valid", `{"reward":0,"valid":true}`, "infra_error", nil, true},
		{"trailing", `{"reward":1,"valid":1} {}`, "infra_error", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := ParseHarborReward([]byte(tt.data), 1)
			if (e != nil) != tt.bad || r.Verdict != tt.verdict {
				t.Fatalf("result=%+v err=%v", r, e)
			}
			if (r.Score == nil) != (tt.score == nil) {
				t.Fatalf("score=%v expected=%v", r.Score, tt.score)
			}
			if r.Score != nil && *r.Score != *tt.score {
				t.Fatalf("score=%v", *r.Score)
			}
		})
	}
}
func number(v float64) *float64 { return &v }
