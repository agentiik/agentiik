package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/agentiik/agentiik/runner"
)

// agentAccount is the account the unit runs the agent as, User=agentiik, and the one join gives
// the key and runner.env to when it runs as root.
const agentAccount = "agentiik"

// join is the verb that trades a join token for this runner's identity: it generates the host's
// key, sends the claim and writes runner.env, as #registering-a-runner describes.
//
// It runs as root on a host being installed, since /etc/agentiik is root's, and it gives what it
// writes to the agent's account, --user, since serve reads runner.env as that account and refuses
// a file owned by any other. Run as that account instead, it writes as itself, and --user is
// refused if it names anybody else, since only root can give a file away.
func join(ctx context.Context, e env, args []string) int {
	fs := flag.NewFlagSet("agk-runner join", flag.ContinueOnError)
	fs.SetOutput(e.Err)
	api := fs.String("api", "", "the address of the API, such as https://agentiik.example.com; "+runner.API+" where not given")
	tokenFlag := fs.String("token", "", "the join token an administrator issued, agkjoin_...")
	labels := fs.String("labels", "", "the labels this runner claims, such as zone=dmz,arch=amd64; "+runner.Labels+" where not given, and none where neither is, as a runner of the pool default claims")
	account := fs.String("user", agentAccount, "the account the agent runs as, which the key and runner.env are given to when join runs as root")
	replace := fs.Bool("replace", false, "replace the identity this host already has with a new runner and a new key")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitSucceeded
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(e.Err, "agk-runner join: it takes flags alone, and the join token is given with --token")
		return exitUsage
	}
	userGiven := false
	fs.Visit(func(f *flag.Flag) { userGiven = userGiven || f.Name == "user" })

	owner, whom, err := ownerOf(e, *account, userGiven)
	if err != nil {
		fmt.Fprintln(e.Err, "agk-runner join: "+err.Error())
		return exitRefused
	}

	socket, err := dockerHost(e)
	if err != nil {
		fmt.Fprintln(e.Err, "agk-runner join: "+err.Error())
		return exitRefused
	}
	joined, err := runner.Join(ctx, runner.Joining{
		API: *api, Token: runner.Secret(*tokenFlag), Labels: *labels,
		Lookup:  e.Lookup,
		Replace: *replace,
		Owner:   owner,
		EnvPath: e.EnvFile, KeyPath: e.KeyFile, MemInfo: e.MemInfo,
		CredentialPath: e.CredentialFile,
		Socket:         socket,
	})
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintln(e.Err, "agk-runner join: "+line)
		}
		return exitRefused
	}
	said(e.Out, joined, e, whom)
	return exitSucceeded
}

// ownerOf is the account join gives its files to, and how to name it.
func ownerOf(e env, account string, given bool) (*runner.Owner, string, error) {
	if e.Geteuid() != 0 {
		if !given {
			return nil, "the account join ran as", nil
		}
		o, err := e.Account(account)
		if err != nil {
			return nil, "", err
		}
		if o.UID != e.Geteuid() {
			return nil, "", fmt.Errorf("--user names account %s, and join runs as account %d, which cannot give a file to another: run join as root, which gives what it writes to --user, or as %s itself", account, e.Geteuid(), account)
		}
		return nil, "account " + account, nil
	}
	o, err := e.Account(account)
	if err != nil {
		return nil, "", err
	}
	if o.UID == 0 {
		return nil, "", fmt.Errorf("--user names account %s, which is root, and serve refuses to run as root: the key and runner.env belong to the unprivileged account the agent runs as", account)
	}
	return &o, "account " + account, nil
}

// lookupAccount is an account of this host by its name, as the unit's User= names it.
func lookupAccount(name string) (runner.Owner, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return runner.Owner{}, fmt.Errorf("the account %s cannot be found on this host, and it is the one the agent runs as, which the key and runner.env are given to: create it, or name another with --user: %v", name, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return runner.Owner{}, fmt.Errorf("the account %s has user id %q, which is not a number", name, u.Uid)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return runner.Owner{}, fmt.Errorf("the account %s has group id %q, which is not a number", name, u.Gid)
	}
	return runner.Owner{UID: uid, GID: gid}, nil
}

// said is what a join that succeeded tells the operator: who the host now is, where what it was
// given is, and until when.
func said(w io.Writer, j runner.Joined, e env, whom string) {
	fmt.Fprintf(w, "This host joined pool %s as runner %s.\n", j.Pool, j.Runner)
	switch {
	case len(j.Labels) == 0 && j.Pool == "default":
		fmt.Fprintln(w, "It claims no label, so it takes the steps that name no runs_on, which go to the pool default.")
	case len(j.Labels) == 0:
		// The API takes a claim of no label to any pool, as a subset of what the token permits,
		// but a step naming no runs_on goes to the pool default, so a runner claiming none
		// anywhere else is handed only steps it puts back. Most likely --labels was left out.
		fmt.Fprintf(w, "It claims no label, and pool %s sends only steps that name one, so it will run none of them: if --labels was left out, join again with --replace, --labels and a new token.\n", j.Pool)
	default:
		fmt.Fprintf(w, "It claims the labels %s.\n", strings.Join(j.Labels, ","))
	}
	fmt.Fprintf(w, "Its key is in %s and its credential in %s, both mode 0600 and owned by %s.\n", e.KeyFile, e.EnvFile, whom)
	fmt.Fprintf(w, "The credential is accepted until %s.\n", j.RotateBy.UTC().Format(time.RFC3339))
	fmt.Fprintln(w, "agk-runner serve starts the agent, as the unit runs it.")
}
