// retire-approvals is an explicit, offline data transition, never a server startup hook.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"oncall-agent/internal/store"
)

func main() {
	apply := flag.Bool("apply", false, "confirm the server is stopped and backed up; retire legacy active approvals")
	check := flag.Bool("check", false, "read-only check that execution migrations and legacy retirement are complete")
	flag.Parse()
	if *apply == *check || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "use exactly one of -check (read-only) or -apply (offline retirement) with MYSQL_DSN; stop server and back up MySQL before -apply")
		os.Exit(2)
	}
	db, err := store.Open(os.Getenv("MYSQL_DSN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if *check {
		if err := db.CheckExecutionReady(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("execution schema and legacy retirement ready (read-only check)")
		return
	}
	count, err := db.RetireLegacyApprovals(ctx, time.Now().UTC())
	if err != nil {
		fmt.Fprintf(os.Stderr, "retirement stopped after %d confirmed committed rows; safe to rerun offline: %v\n", count, err)
		os.Exit(1)
	}
	fmt.Printf("retired %d legacy active approvals; terminal history and modern snapshots unchanged\n", count)
}
