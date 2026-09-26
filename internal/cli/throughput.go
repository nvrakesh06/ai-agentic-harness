package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/nvrakesh06/ai-agentic-harness/internal/engine"
	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
	"github.com/spf13/cobra"
)

func throughputCommand(o *options) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "throughput", Short: "Report observed task timing and pinned provider repetition", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		p, err := o.open(cmd.Context(), false)
		if err != nil {
			return err
		}
		defer p.DB.Close()
		return showThroughput(cmd, p, asJSON, time.Now().UTC())
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable observed metrics")
	return cmd
}

func showThroughput(cmd *cobra.Command, p *engine.Project, asJSON bool, at time.Time) error {
	s, ref, err := p.DB.Load()
	if err != nil {
		return fmt.Errorf("no cached state; run aih attach: %w", err)
	}
	live := active(p)
	report := struct {
		model.ThroughputReport
		StateRef              string `json:"state_ref"`
		Revision              uint64 `json:"revision"`
		LocalSupervisorActive bool   `json:"local_supervisor_active"`
		LocalActiveWriters    int    `json:"local_active_writers"`
		TargetWriters         int    `json:"target_active_writers"`
		Reason                string `json:"capacity_reason"`
	}{model.Throughput(s, at), ref, s.Revision, live, localCount(live, s.Capacity.ActiveWriters), s.Capacity.TargetWriters, s.Capacity.ReasonCode}
	if !live {
		report.Reason = "supervisor_stopped"
	}
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Throughput at %s · cached revision %d\n", at.Format(time.RFC3339), s.Revision)
	fmt.Fprintf(cmd.OutOrStdout(), "Local supervisor active: %t · writers %d/%d · %s\n", live, report.LocalActiveWriters, report.TargetWriters, report.Reason)
	fmt.Fprintln(cmd.OutOrStdout(), "Provider time is summed invocation time, not wall time or writer utilization. Historical writer utilization is unavailable.")
	for _, task := range report.Tasks {
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s · %d runs · provider %s", task.Task, task.State, task.Runs, time.Duration(task.ProviderMS)*time.Millisecond)
		if task.ActiveRuns > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), " · %d unfinished invocations", task.ActiveRuns)
		}
		if task.UnavailableDurations > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), " · %d durations unavailable", task.UnavailableDurations)
		}
		if task.EstimatedDurations > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), " · %d durations estimated", task.EstimatedDurations)
		}
		if task.Timing == nil {
			fmt.Fprintln(cmd.OutOrStdout(), " · state timing unavailable")
			continue
		}
		var residence int64
		for _, duration := range task.Timing.StateMS {
			residence += duration
		}
		fmt.Fprintf(cmd.OutOrStdout(), " · lease-owned state %s · operator wait %s · stopped %s", time.Duration(residence)*time.Millisecond, time.Duration(task.Timing.OperatorWaitMS)*time.Millisecond, time.Duration(task.Timing.StoppedMS)*time.Millisecond)
		if task.Timing.PartialHistory {
			fmt.Fprint(cmd.OutOrStdout(), " · partial history")
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}
	for _, repeat := range report.RepeatedRoles {
		fmt.Fprintf(cmd.OutOrStdout(), "Repeated pinned role: %s %s/%s head %s · %d runs\n", repeat.Task, repeat.Context.Stage, repeat.Role, shortSHA(repeat.Context.Head), repeat.Runs)
	}
	if report.UnavailableRunContexts > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Pinned source context unavailable for %d historical run(s).\n", report.UnavailableRunContexts)
	}
	return nil
}
