package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/fortaegis/plasma/internal/plasma"
)

func engineCmd() *cobra.Command {
	e := &cobra.Command{
		Use:   "engine",
		Short: "Choose and inspect the engine plasma talks to",
		Long: "By default plasma talks to the plasmad on this host. To use one elsewhere,\n" +
			"log in to it once (on the engine, `sudo plasmad --print-login` prints the\n" +
			"command), or set PLASMA_ENGINE, PLASMA_TOKEN and PLASMA_FINGERPRINT.\n" +
			"PLASMA_ENGINE=local forces the local daemon.",
	}
	e.AddCommand(engineLoginCmd(), engineLogoutCmd(), engineInfoCmd())
	return e
}

func engineLoginCmd() *cobra.Command {
	var (
		token       string
		fingerprint string
		caFile      string
		yes         bool
	)
	cmd := &cobra.Command{
		Use:   "login https://<engine>[:port] --token <token> [--fingerprint sha256:...]",
		Short: "Use a remote engine from now on",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			url := args[0]
			if !strings.Contains(url, "://") {
				url = "https://" + url
			}
			if token == "" {
				token = os.Getenv("PLASMA_TOKEN")
			}
			if fingerprint == "" && caFile == "" {
				fp, err := plasma.FetchFingerprint(url)
				if err != nil {
					return fmt.Errorf("reach %s: %w", url, err)
				}
				fmt.Printf("engine %s presents certificate %s\n", url, fp)
				if !yes && !confirm("Is this the fingerprint `sudo plasmad --print-login` shows on the engine? [y/N] ") {
					return errors.New("not trusted; nothing saved")
				}
				fingerprint = fp
			}
			cfg := plasma.EngineConfig{URL: url, Token: token, Fingerprint: fingerprint, CAFile: caFile}
			cl, err := plasma.NewRemoteClient(cfg)
			if err != nil {
				return err
			}
			info, err := cl.Engine(context.Background())
			if err != nil {
				return err
			}
			cfg.URL = cl.Where()
			p, err := plasma.SaveEngineConfig(cfg)
			if err != nil {
				return err
			}
			fmt.Printf("logged in to %s (saved in %s)\n", cfg.URL, p)
			printEngine(info)
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "the engine's token (default: $PLASMA_TOKEN)")
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "pin the engine's certificate (sha256:...)")
	cmd.Flags().StringVar(&caFile, "ca", "", "trust the engine through this CA instead of a pin")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "trust the certificate the engine presents without asking")
	return cmd
}

func confirm(prompt string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Println("not a terminal: pass --fingerprint (or --yes)")
		return false
	}
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

func engineLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Go back to the local engine",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			c.SilenceUsage = true
			p, err := plasma.ConfigPath()
			if err != nil {
				return err
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
			fmt.Println("using the local engine")
			return nil
		},
	}
}

func engineInfoCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Show the engine: guest kernel, build image, capacity",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			c.SilenceUsage = true
			cl := newClient()
			info, err := cl.Engine(context.Background())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(info)
			}
			fmt.Printf("engine %s\n", cl.Where())
			printEngine(info)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func printEngine(i plasma.EngineInfo) {
	gib := func(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }
	fmt.Printf("  version       %s (%s)\n", i.Version, i.Arch)
	fmt.Printf("  guest kernel  %s\n", orDash(i.GuestKernel))
	if i.KernelHeaders != "" {
		fmt.Printf("  headers       %s\n", i.KernelHeaders)
	}
	fmt.Printf("  kbuild image  %s\n", orDash(i.KbuildImage))
	fmt.Printf("  capacity      %d cpu, %s free of %s (%d lab(s), %d/%d VM(s) running)\n",
		i.Capacity.CPUs, gib(i.Capacity.MemoryFree), gib(i.Capacity.MemoryTotal),
		i.Capacity.Labs, i.Capacity.VMsRunning, i.Capacity.VMs)
	fmt.Printf("  artifacts     %d cached, %s\n", i.Artifacts.Count, gib(i.Artifacts.Bytes))
}

func artifactCmd() *cobra.Command {
	a := &cobra.Command{Use: "artifact", Short: "Upload files to the engine's artifact store"}
	a.AddCommand(&cobra.Command{
		Use:   "push <file>...",
		Short: "Upload files (skipping any the engine has) and print their references",
		Long: "Prints sha256:<digest> for each file, for use as `artifact:` in a lab\n" +
			"topology. `plasma lab apply` does this itself for files a topology names.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			c.SilenceUsage = true
			cl := newClient()
			for _, f := range args {
				d, sent, err := cl.PushArtifact(context.Background(), f)
				if err != nil {
					return fmt.Errorf("%s: %w", f, err)
				}
				state := "cached"
				if sent {
					state = "uploaded"
				}
				fmt.Printf("%s  %s  (%s)\n", plasma.ArtifactRef(d), f, state)
			}
			return nil
		},
	})
	return a
}
