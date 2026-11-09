package cli

import (
	"fmt"
	"strconv"
	"strings"
)

type options struct {
	path       string
	configPath string
	dbPath     string
	logLevel   string
	output     string
	days       int
	json       bool
	logical    bool
	full       bool
	resume     bool
	once       bool
	vacuum     bool
	repair     bool
	help       bool
	version    bool
	positional []string
}

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func parseArgs(args []string) (*options, error) {
	opts := &options{days: -1}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			opts.positional = append(opts.positional, args[i+1:]...)
			break
		}
		switch {
		case arg == "--json":
			opts.json = true
		case arg == "--logical":
			opts.logical = true
		case arg == "--full":
			opts.full = true
		case arg == "--resume":
			opts.resume = true
		case arg == "--once":
			opts.once = true
		case arg == "--vacuum":
			opts.vacuum = true
		case arg == "--repair":
			opts.repair = true
		case arg == "-h" || arg == "--help":
			opts.help = true
		case arg == "--version" || arg == "-v":
			opts.version = true
		case arg == "--path" || arg == "--config" || arg == "--db" ||
			arg == "--log-level" || arg == "--output" || arg == "--days":
			if i+1 >= len(args) {
				return nil, &usageError{fmt.Sprintf("%s 需要一个参数", arg)}
			}
			i++
			if err := assignOption(opts, arg, args[i]); err != nil {
				return nil, err
			}
		case strings.HasPrefix(arg, "--path="):
			opts.path = strings.TrimPrefix(arg, "--path=")
		case strings.HasPrefix(arg, "--config="):
			opts.configPath = strings.TrimPrefix(arg, "--config=")
		case strings.HasPrefix(arg, "--db="):
			opts.dbPath = strings.TrimPrefix(arg, "--db=")
		case strings.HasPrefix(arg, "--log-level="):
			opts.logLevel = strings.TrimPrefix(arg, "--log-level=")
		case strings.HasPrefix(arg, "--output="):
			opts.output = strings.TrimPrefix(arg, "--output=")
		case strings.HasPrefix(arg, "--days="):
			value := strings.TrimPrefix(arg, "--days=")
			days, err := strconv.Atoi(value)
			if err != nil || days < 1 {
				return nil, &usageError{fmt.Sprintf("--days 需要正整数，收到 %q", value)}
			}
			opts.days = days
		case strings.HasPrefix(arg, "-"):
			return nil, &usageError{fmt.Sprintf("未知参数: %s", arg)}
		default:
			opts.positional = append(opts.positional, arg)
		}
	}
	return opts, nil
}

func assignOption(opts *options, name, value string) error {
	switch name {
	case "--path":
		opts.path = value
	case "--config":
		opts.configPath = value
	case "--db":
		opts.dbPath = value
	case "--log-level":
		opts.logLevel = value
	case "--output":
		opts.output = value
	case "--days":
		days, err := strconv.Atoi(value)
		if err != nil || days < 1 {
			return &usageError{fmt.Sprintf("--days 需要正整数，收到 %q", value)}
		}
		opts.days = days
	default:
		return &usageError{fmt.Sprintf("未知参数: %s", name)}
	}
	return nil
}

func parsePositiveInt(value, what string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, &usageError{fmt.Sprintf("%s 需要正整数，收到 %q", what, value)}
	}
	return n, nil
}
