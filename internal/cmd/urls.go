package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/xerrors"

	"cdr.dev/coder-cli/coder-sdk"
	"cdr.dev/coder-cli/pkg/clog"
	"cdr.dev/coder-cli/pkg/tablewriter"
)

func urlCmd() *cobra.Command {
	var outputFmt string
	cmd := &cobra.Command{
		Use:   "urls",
		Short: "Interact with environment DevURLs",
	}
	lsCmd := &cobra.Command{
		Use:               "ls [environment_name]",
		Short:             "List all DevURLs for an environment",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: getEnvsForCompletion(coder.Me),
		RunE:              listDevURLsCmd(&outputFmt),
	}
	lsCmd.Flags().StringVarP(&outputFmt, "output", "o", humanOutput, "human|json")

	rmCmd := &cobra.Command{
		Use:   "rm [environment_name] [port]",
		Args:  cobra.ExactArgs(2),
		Short: "Remove a dev url",
		RunE:  removeDevURL,
	}

	cmd.AddCommand(
		lsCmd,
		rmCmd,
		createDevURLCmd(),
	)

	return cmd
}

// DevURL is the parsed json response record for a devURL from cemanager.
type DevURL struct {
	ID     string `json:"id"     table:"-"`
	URL    string `json:"url"    table:"URL"`
	Port   int    `json:"port"   table:"Port"`
	Name   string `json:"name"   table:"-"`
	Access string `json:"access" table:"Access"`
}

var urlAccessLevel = map[string]string{
	// Remote API endpoint requires these in uppercase.
	"PRIVATE": "Only you can access",
	"ORG":     "All members of your organization can access",
	"AUTHED":  "Authenticated users can access",
	"PUBLIC":  "Anyone on the internet can access this link",
}

func validatePort(port string) (int, error) {
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		clog.Log(clog.Error("invalid port"))
		return 0, err
	}
	if p < 1 {
		// Port 0 means 'any free port', which we don't support.
		return 0, xerrors.New("Port must be > 0")
	}
	return int(p), nil
}

func accessLevelIsValid(level string) bool {
	_, ok := urlAccessLevel[level]
	if !ok {
		clog.Log(clog.Error("invalid access level"))
	}
	return ok
}

// Run gets the list of active devURLs from the cemanager for the
// specified environment and outputs info to stdout.
func listDevURLsCmd(outputFmt *string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		client, err := newClient(ctx)
		if err != nil {
			return err
		}
		envName := args[0]

		devURLs, err := urlList(ctx, client, envName)
		if err != nil {
			return err
		}

		switch *outputFmt {
		case humanOutput:
			if len(devURLs) < 1 {
				clog.LogInfo(fmt.Sprintf("no devURLs found for environment %q", envName))
				return nil
			}
			err := tablewriter.WriteTable(len(devURLs), func(i int) interface{} {
				return devURLs[i]
			})
			if err != nil {
				return xerrors.Errorf("write table: %w", err)
			}
		case jsonOutput:
			if err := json.NewEncoder(os.Stdout).Encode(devURLs); err != nil {
				return xerrors.Errorf("encode DevURLs as json: %w", err)
			}
		default:
			return xerrors.Errorf("unknown --output value %q", *outputFmt)
		}
		return nil
	}
}

func createDevURLCmd() *cobra.Command {
	var (
		access  string
		urlname string
	)
	cmd := &cobra.Command{
		Use:     "create [env_name] [port] [--access <level>] [--name <name>]",
		Short:   "Create a new devurl for an environment",
		Aliases: []string{"edit"},
		Args:    cobra.ExactArgs(2),
		// Run creates or updates a devURL
		RunE: func(cmd *cobra.Command, args []string) error {
			var (
				envName = args[0]
				port    = args[1]
				ctx     = cmd.Context()
			)

			portNum, err := validatePort(port)
			if err != nil {
				return err
			}

			access = strings.ToUpper(access)
			if !accessLevelIsValid(access) {
				return xerrors.Errorf("invalid access level %q", access)
			}

			if urlname != "" && !devURLNameValidRx.MatchString(urlname) {
				return xerrors.New("update devurl: name must be < 64 chars in length, begin with a letter and only contain letters or digits.")
			}
			client, err := newClient(ctx)
			if err != nil {
				return err
			}

			env, err := findEnv(ctx, client, envName, coder.Me)
			if err != nil {
				return err
			}

			urls, err := urlList(ctx, client, envName)
			if err != nil {
				return err
			}

			urlID, found := devURLID(portNum, urls)
			if found {
				clog.LogInfo(fmt.Sprintf("updating devurl for port %v", port))
				err := client.PutDevURL(ctx, env.ID, urlID, coder.PutDevURLReq{
					Port:   portNum,
					Name:   urlname,
					Access: access,
					EnvID:  env.ID,
					Scheme: "http",
				})
				if err != nil {
					return xerrors.Errorf("update DevURL: %w", err)
				}
			} else {
				clog.LogInfo(fmt.Sprintf("Adding devurl for port %v", port))
				err := client.CreateDevURL(ctx, env.ID, coder.CreateDevURLReq{
					Port:   portNum,
					Name:   urlname,
					Access: access,
					EnvID:  env.ID,
					Scheme: "http",
				})
				if err != nil {
					return xerrors.Errorf("insert DevURL: %w", err)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&access, "access", "private", "Set DevURL access to [private | org | authed | public]")
	cmd.Flags().StringVar(&urlname, "name", "", "DevURL name")
	_ = cmd.MarkFlagRequired("name")

	return cmd
}

// devURLNameValidRx is the regex used to validate devurl names specified
// via the --name subcommand. Named devurls must begin with a letter, and
// consist solely of letters and digits, with a max length of 64 chars.
var devURLNameValidRx = regexp.MustCompile("^[a-zA-Z][a-zA-Z0-9]{0,63}$")

// devURLID returns the ID of a devURL, given the env name and port
// from a list of DevURL records.
// ("", false) is returned if no match is found.
func devURLID(port int, urls []DevURL) (string, bool) {
	for _, url := range urls {
		if url.Port == port {
			return url.ID, true
		}
	}
	return "", false
}

// Run deletes a devURL, specified by env ID and port, from the cemanager.
func removeDevURL(cmd *cobra.Command, args []string) error {
	var (
		envName = args[0]
		port    = args[1]
		ctx     = cmd.Context()
	)

	portNum, err := validatePort(port)
	if err != nil {
		return xerrors.Errorf("validate port: %w", err)
	}

	client, err := newClient(ctx)
	if err != nil {
		return err
	}
	env, err := findEnv(ctx, client, envName, coder.Me)
	if err != nil {
		return err
	}

	urls, err := urlList(ctx, client, envName)
	if err != nil {
		return err
	}

	urlID, found := devURLID(portNum, urls)
	if found {
		clog.LogInfo(fmt.Sprintf("deleting devurl for port %v", port))
	} else {
		return xerrors.Errorf("No devurl found for port %v", port)
	}

	if err := client.DeleteDevURL(ctx, env.ID, urlID); err != nil {
		return xerrors.Errorf("delete DevURL: %w", err)
	}
	return nil
}

// urlList returns the list of active devURLs from the cemanager.
func urlList(ctx context.Context, client *coder.Client, envName string) ([]DevURL, error) {
	env, err := findEnv(ctx, client, envName, coder.Me)
	if err != nil {
		return nil, err
	}

	reqString := "%s/api/environments/%s/devurls?session_token=%s"
	reqURL := fmt.Sprintf(reqString, client.BaseURL, env.ID, client.Token)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() // Best effort.

	if resp.StatusCode != http.StatusOK {
		return nil, xerrors.Errorf("non-success status code: %d", resp.StatusCode)
	}

	dec := json.NewDecoder(resp.Body)

	var devURLs []DevURL
	if err := dec.Decode(&devURLs); err != nil {
		return nil, err
	}

	return devURLs, nil
}
