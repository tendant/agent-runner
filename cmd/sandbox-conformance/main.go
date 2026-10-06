// Command sandbox-conformance runs the shared conformance suite against the
// isobox backend on THIS host and writes the evidence report plus a capability
// manifest containing only proven capabilities.
//
//	sandbox-conformance --isobox ./isobox --backend gvisor --report gvisor-linux.json --manifest production.json
//
// Point AGENT_SANDBOX_EVIDENCE at the report (so only proven capabilities are
// claimed) and use the manifest for offline checks on other machines.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/agent-runner/agent-runner/internal/sandbox/conformance"
	"github.com/agent-runner/agent-runner/internal/sandbox/isobox"
)

func main() {
	bin := flag.String("isobox", "isobox", "path to the isobox binary")
	backend := flag.String("backend", "", "isobox backend: seatbelt | gvisor (empty = native)")
	report := flag.String("report", "conformance-report.json", "evidence report output")
	manifest := flag.String("manifest", "", "capability manifest output (proven capabilities only)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	name := "isobox:" + *backend
	rep, err := conformance.Run(ctx, name, func(e conformance.Env) sandbox.Backend {
		return isobox.New(isobox.Config{Binary: *bin, Backend: *backend, Roots: e.Roots})
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*report, rep.JSON(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	proven := rep.ProvenSet()
	fmt.Printf("%s on %s: %d capabilities proven\n", name, rep.Host, len(proven))
	for id, r := range rep.Results {
		if r.Status != conformance.Pass {
			fmt.Printf("  %-8s %-34s %s\n", r.Status, id, r.Detail)
		}
	}
	if *manifest != "" {
		m := sandbox.Manifest{Backend: name, RegistryVersion: sandbox.RegistryVersion, Capabilities: proven,
			Caveats: []string{"generated from conformance evidence on " + rep.Host}}
		b, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(*manifest, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
