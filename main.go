package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/terraform-svchost/disco"
	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/command/cliconfig"
	"github.com/hashicorp/terraform/command/format"
	"github.com/hashicorp/terraform/helper/logging"
	"github.com/hashicorp/terraform/httpclient"
	"github.com/hashicorp/terraform/version"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-shellwords"
	"github.com/mitchellh/cli"
	"github.com/mitchellh/colorstring"
	"github.com/mitchellh/panicwrap"
	"github.com/mitchellh/prefixedio"

	backendInit "github.com/hashicorp/terraform/backend/init"
)

const (
	// EnvCLI is the environment variable name to set additional CLI args.
	EnvCLI = "TF_CLI_ARGS"
)

func main() {
	// Override global prefix set by go-dynect during init()
	log.SetPrefix("")
	os.Exit(realMain())
}

func realMain() int {
	var wrapConfig panicwrap.WrapConfig

	// don't re-exec terraform as a child process for easier debugging
	if os.Getenv("TF_FORK") == "0" {
		return wrappedMain()
	}

	if !panicwrap.Wrapped(&wrapConfig) {
		// Determine where logs should go in general (requested by the user)
		logWriter, err := logging.LogOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't setup log output: %s", err)
			return 1
		}

		// We always send logs to a temporary file that we use in case
		// there is a panic. Otherwise, we delete it.
		logTempFile, err := ioutil.TempFile("", "terraform-log")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't setup logging tempfile: %s", err)
			return 1
		}
		defer os.Remove(logTempFile.Name())
		defer logTempFile.Close()

		// Setup the prefixed readers that send data properly to
		// stdout/stderr.
		doneCh := make(chan struct{})
		outR, outW := io.Pipe()
		go copyOutput(outR, doneCh)

		// Create the configuration for panicwrap and wrap our executable
		wrapConfig.Handler = panicHandler(logTempFile)
		wrapConfig.Writer = io.MultiWriter(logTempFile, logWriter)
		wrapConfig.Stdout = outW
		wrapConfig.IgnoreSignals = ignoreSignals
		wrapConfig.ForwardSignals = forwardSignals
		exitStatus, err := panicwrap.Wrap(&wrapConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't start Terraform: %s", err)
			return 1
		}

		// If >= 0, we're the parent, so just exit
		if exitStatus >= 0 {
			// Close the stdout writer so that our copy process can finish
			outW.Close()

			// Wait for the output copying to finish
			<-doneCh

			return exitStatus
		}

		// We're the child, so just close the tempfile we made in order to
		// save file handles since the tempfile is only used by the parent.
		logTempFile.Close()
	}

	// Call the real main
	return wrappedMain()
}

func init() {
	Ui = &cli.PrefixedUi{
		AskPrefix:    OutputPrefix,
		OutputPrefix: OutputPrefix,
		InfoPrefix:   OutputPrefix,
		ErrorPrefix:  ErrorPrefix,
		Ui: &cli.BasicUi{
			Writer: os.Stdout,
			Reader: os.Stdin,
		},
	}
}

func wrappedMain() int {
	var err error

	log.SetOutput(os.Stderr)
	log.Printf(
		"[INFO] Terraform version: %s %s %s",
		Version, VersionPrerelease, GitCommit)
	log.Printf("[INFO] Go runtime version: %s", runtime.Version())
	log.Printf("[INFO] CLI args: %#v", os.Args)

	config, diags := cliconfig.LoadConfig()
	if len(diags) > 0 {
		// Since we haven't instantiated a command.Meta yet, we need to do
		// some things manually here and use some "safe" defaults for things
		// that command.Meta could otherwise figure out in smarter ways.
		Ui.Error("There are some problems with the CLI configuration:")
		for _, diag := range diags {
			earlyColor := &colorstring.Colorize{
				Colors:  colorstring.DefaultColors,
				Disable: true, // Disable color to be conservative until we know better
				Reset:   true,
			}
			// We don't currently have access to the source code cache for
			// the parser used to load the CLI config, so we can't show
			// source code snippets in early diagnostics.
			Ui.Error(format.Diagnostic(diag, nil, earlyColor, 78))
		}
		if diags.HasErrors() {
			Ui.Error("As a result of the above problems, Terraform may not behave as intended.\n\n")
			// We continue to run anyway, since Terraform has reasonable defaults.
		}
	}

	// Get any configured credentials from the config and initialize
	// a service discovery object.
	credsSrc, err := credentialsSource(config)
	if err != nil {
		// Most commands don't actually need credentials, and most situations
		// that would get us here would already have been reported by the config
		// loading above, so we'll just log this one as an aid to debugging
		// in the unlikely event that it _does_ arise.
		log.Printf("[WARN] Cannot initialize remote host credentials manager: %s", err)
		// credsSrc may be nil in this case, but that's okay because the disco
		// object checks that and just acts as though no credentials are present.
	}
	services := disco.NewWithCredentialsSource(credsSrc)
	services.SetUserAgent(httpclient.TerraformUserAgent(version.String()))

	// The user can declare that certain providers are being managed on
	// Terraform's behalf using this environment variable. Thsi is used
	// primarily by the SDK's acceptance testing framework.
	unmanagedProviders, err := parseReattachProviders(os.Getenv("TF_REATTACH_PROVIDERS"))
	if err != nil {
		Ui.Error(err.Error())
		return 1
	}

	// Initialize the backends.
	backendInit.Init(services)

	// In tests, Commands may already be set to provide mock commands
	if Commands == nil {
		initCommands(config, services, unmanagedProviders)
	}

	// Run checkpoint
	go runCheckpoint(config)

	// Make sure we clean up any managed plugins at the end of this
	defer plugin.CleanupClients()

	// Get the command line args.
	binName := filepath.Base(os.Args[0])
	args := os.Args[1:]

	// Build the CLI so far, we do this so we can query the subcommand.
	cliRunner := &cli.CLI{
		Args:       args,
		Commands:   Commands,
		HelpFunc:   helpFunc,
		HelpWriter: os.Stdout,
	}

	// Prefix the args with any args from the EnvCLI
	args, err = mergeEnvArgs(EnvCLI, cliRunner.Subcommand(), args)
	if err != nil {
		Ui.Error(err.Error())
		return 1
	}

	// Prefix the args with any args from the EnvCLI targeting this command
	suffix := strings.Replace(strings.Replace(
		cliRunner.Subcommand(), "-", "_", -1), " ", "_", -1)
	args, err = mergeEnvArgs(
		fmt.Sprintf("%s_%s", EnvCLI, suffix), cliRunner.Subcommand(), args)
	if err != nil {
		Ui.Error(err.Error())
		return 1
	}

	// We shortcut "--version" and "-v" to just show the version
	for _, arg := range args {
		if arg == "-v" || arg == "-version" || arg == "--version" {
			newArgs := make([]string, len(args)+1)
			newArgs[0] = "version"
			copy(newArgs[1:], args)
			args = newArgs
			break
		}
	}

	// Rebuild the CLI with any modified args.
	log.Printf("[INFO] CLI command args: %#v", args)
	cliRunner = &cli.CLI{
		Name:       binName,
		Args:       args,
		Commands:   Commands,
		HelpFunc:   helpFunc,
		HelpWriter: os.Stdout,

		Autocomplete:          true,
		AutocompleteInstall:   "install-autocomplete",
		AutocompleteUninstall: "uninstall-autocomplete",
	}

	// Pass in the overriding plugin paths from config
	PluginOverrides.Providers = config.Providers
	PluginOverrides.Provisioners = config.Provisioners

	exitCode, err := cliRunner.Run()
	if err != nil {
		Ui.Error(fmt.Sprintf("Error executing CLI: %s", err.Error()))
		return 1
	}

	return exitCode
}

// copyOutput uses output prefixes to determine whether data on stdout
// should go to stdout or stderr. This is due to panicwrap using stderr
// as the log and error channel.
func copyOutput(r io.Reader, doneCh chan<- struct{}) {
	defer close(doneCh)

	pr, err := prefixedio.NewReader(r)
	if err != nil {
		panic(err)
	}

	stderrR, err := pr.Prefix(ErrorPrefix)
	if err != nil {
		panic(err)
	}
	stdoutR, err := pr.Prefix(OutputPrefix)
	if err != nil {
		panic(err)
	}
	defaultR, err := pr.Prefix("")
	if err != nil {
		panic(err)
	}

	var stdout io.Writer = os.Stdout
	var stderr io.Writer = os.Stderr

	if runtime.GOOS == "windows" {
		stdout = colorable.NewColorableStdout()
		stderr = colorable.NewColorableStderr()

		// colorable is not concurrency-safe when stdout and stderr are the
		// same console, so we need to add some synchronization to ensure that
		// we can't be concurrently writing to both stderr and stdout at
		// once, or else we get intermingled writes that create gibberish
		// in the console.
		wrapped := synchronizedWriters(stdout, stderr)
		stdout = wrapped[0]
		stderr = wrapped[1]
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		io.Copy(stderr, stderrR)
	}()
	go func() {
		defer wg.Done()
		io.Copy(stdout, stdoutR)
	}()
	go func() {
		defer wg.Done()
		io.Copy(stdout, defaultR)
	}()

	wg.Wait()
}

func mergeEnvArgs(envName string, cmd string, args []string) ([]string, error) {
	v := os.Getenv(envName)
	if v == "" {
		return args, nil
	}

	log.Printf("[INFO] %s value: %q", envName, v)
	extra, err := shellwords.Parse(v)
	if err != nil {
		return nil, fmt.Errorf(
			"Error parsing extra CLI args from %s: %s",
			envName, err)
	}

	// Find the command to look for in the args. If there is a space,
	// we need to find the last part.
	search := cmd
	if idx := strings.LastIndex(search, " "); idx >= 0 {
		search = cmd[idx+1:]
	}

	// Find the index to place the flags. We put them exactly
	// after the first non-flag arg.
	idx := -1
	for i, v := range args {
		if v == search {
			idx = i
			break
		}
	}

	// idx points to the exact arg that isn't a flag. We increment
	// by one so that all the copying below expects idx to be the
	// insertion point.
	idx++

	// Copy the args
	newArgs := make([]string, len(args)+len(extra))
	copy(newArgs, args[:idx])
	copy(newArgs[idx:], extra)
	copy(newArgs[len(extra)+idx:], args[idx:])
	return newArgs, nil
}

// parse information on reattaching to unmanaged providers out of a
// JSON-encoded environment variable.
func parseReattachProviders(in string) (map[addrs.Provider]*plugin.ReattachConfig, error) {
	unmanagedProviders := map[addrs.Provider]*plugin.ReattachConfig{}
	if in != "" {
		type reattachConfig struct {
			Protocol string
			Addr     struct {
				Network string
				String  string
			}
			Pid  int
			Test bool
		}
		var m map[string]reattachConfig
		err := json.Unmarshal([]byte(in), &m)
		if err != nil {
			return unmanagedProviders, fmt.Errorf("Invalid format for TF_REATTACH_PROVIDERS: %s", err)
		}
		for p, c := range m {
			a, diags := addrs.ParseProviderSourceString(p)
			if diags.HasErrors() {
				return unmanagedProviders, fmt.Errorf("Error parsing %q as a provider address: %s", a, diags.Err())
			}
			var addr net.Addr
			switch c.Addr.Network {
			case "unix":
				addr, err = net.ResolveUnixAddr("unix", c.Addr.String)
				if err != nil {
					return unmanagedProviders, fmt.Errorf("Invalid unix socket path %q for %q: %s", c.Addr.String, p, err)
				}
			case "tcp":
				addr, err = net.ResolveTCPAddr("tcp", c.Addr.String)
				if err != nil {
					return unmanagedProviders, fmt.Errorf("Invalid TCP address %q for %q: %s", c.Addr.String, p, err)
				}
			default:
				return unmanagedProviders, fmt.Errorf("Unknown address type %q for %q", c.Addr.Network, p)
			}
			unmanagedProviders[a] = &plugin.ReattachConfig{
				Protocol: plugin.Protocol(c.Protocol),
				Pid:      c.Pid,
				Test:     c.Test,
				Addr:     addr,
			}
		}
	}
	return unmanagedProviders, nil
}
