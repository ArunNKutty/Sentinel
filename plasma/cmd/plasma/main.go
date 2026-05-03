package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/fortaegis/plasma/internal/plasma"
)

func main() {
	root := &cobra.Command{
		Use:           "plasma",
		Short:         "Control plane CLI for the NE simulation platform",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	vm := &cobra.Command{Use: "vm", Short: "VM lifecycle commands"}
	vm.AddCommand(launchCmd(), listCmd())
	root.AddCommand(vm)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func launchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "launch <group>",
		Short: "Launch a new VM in the given group",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := plasma.NewClient(plasma.SocketPath)
			rec, err := c.Launch(args[0])
			if err != nil {
				return err
			}
			fmt.Printf("launched %s (pid %d)\n", rec.ID, rec.PID)
			return nil
		},
	}
}

func listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List VMs",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c := plasma.NewClient(plasma.SocketPath)
			vms, err := c.List()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tGROUP\tSTATUS\tIP\tPID\tUPTIME")
			for _, v := range vms {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
					v.ID, v.Group, v.Status, v.IP, v.PID, uptime(v.StartedAt))
			}
			return tw.Flush()
		},
	}
}

func uptime(start time.Time) string {
	d := time.Since(start).Round(time.Second)
	return d.String()
}
