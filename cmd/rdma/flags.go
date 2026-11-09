package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// usageError is a command line that makes no sense; it is reported with
// exit status 2.
type usageError string

func (e usageError) Error() string { return string(e) }

// newFlagSet returns a flag set that reports errors through its return value
// and prints nothing itself.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses args with fs and then runs check; -h yields flag.ErrHelp
// and anything else wrong with the command line a usageError.
func parseFlags(fs *flag.FlagSet, args []string, check func() error) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError(err.Error())
	}
	if fs.NArg() > 0 {
		return usageError(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if check != nil {
		if err := check(); err != nil {
			return usageError(err.Error())
		}
	}
	return nil
}

// reportParse prints what an error from parseFlags calls for and returns the
// exit status: the usage on stdout for -h, the error and where to find the
// usage on stderr otherwise.
func reportParse(err error, fs *flag.FlagSet, usage string, stdout, stderr io.Writer) int {
	if errors.Is(err, flag.ErrHelp) {
		printUsage(stdout, fs, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "rdma %s: %v\n", fs.Name(), err)
	fmt.Fprintf(stderr, "Run 'rdma help %s' for usage.\n", fs.Name())
	return exitUsage
}

// printUsage prints the usage text followed by the flags of fs, if any.
func printUsage(w io.Writer, fs *flag.FlagSet, usage string) {
	fmt.Fprint(w, usage)
	n := 0
	fs.VisitAll(func(*flag.Flag) { n++ })
	if n == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
}

// fail reports a run-time error of a command and returns the exit status.
func fail(name string, err error, stderr io.Writer) int {
	fmt.Fprintf(stderr, "rdma %s: %v\n", name, err)
	return exitError
}
