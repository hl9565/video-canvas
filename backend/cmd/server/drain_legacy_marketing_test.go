package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunDrainLegacyMarketing_RequiresAllThreePreconditions(t *testing.T) {
	cases := [][]string{
		{},
		{"-legacy-claim-stopped"},
		{"-legacy-claim-stopped", "-legacy-inflight-settled"},
		{"-legacy-inflight-settled", "-legacy-workers-exited"},
	}
	for _, args := range cases {
		var out bytes.Buffer
		err := runDrainLegacyMarketing(args, "../../configs/config.yaml", &out)
		if err == nil || !strings.Contains(err.Error(), "前置条件") {
			t.Fatalf("args=%v 未确认全部三项前置条件必须拒绝执行：err=%v", args, err)
		}
	}
}
