package cli

import (
	"fmt"
	"strconv"
	"strings"
)

type options struct {
	path        string
	configPath  string
	dbPath      string
	logLevel    string
	output      string
	days        int
	json        bool
	logical     bool
	full        bool
	newOnly     bool
	resume      bool
	stagingOnly bool
	exact       bool
	shard       string
	inputs      []string
	once        bool
	vacuum      bool
	repair      bool
	help        bool
	version     bool
	positional  []string
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
		case arg == "--new":
			opts.newOnly = true
		case arg == "--resume":
			opts.resume = true
		case arg == "--staging-only":
			opts.stagingOnly = true
		case arg == "--exact":
			opts.exact = true
		case arg == "--shard":
			if i+1 >= len(args) {
				return nil, &usageError{fmt.Sprintf("%s 需要一个参数", arg)}
			}
			i++
			opts.shard = args[i]
		case arg == "--inputs":
			if i+1 >= len(args) {
				return nil, &usageError{fmt.Sprintf("%s 需要一个参数", arg)}
			}
			i++
			opts.inputs = append(opts.inputs, splitInputs(args[i])...)
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
		case strings.HasPrefix(arg, "--shard="):
			opts.shard = strings.TrimPrefix(arg, "--shard=")
		case strings.HasPrefix(arg, "--inputs="):
			opts.inputs = append(opts.inputs, splitInputs(strings.TrimPrefix(arg, "--inputs="))...)
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
	case "--shard":
		opts.shard = value
	case "--inputs":
		opts.inputs = append(opts.inputs, splitInputs(value)...)
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

func parseShard(value string) (index, count int, err error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return 0, 0, &usageError{"--shard 需要 INDEX/COUNT，例如 1/4"}
	}
	index, err = strconv.Atoi(parts[0])
	if err != nil || index < 0 {
		return 0, 0, &usageError{"--shard 的 INDEX 必须是非负整数"}
	}
	count, err = strconv.Atoi(parts[1])
	if err != nil || count < 1 || count > 64 {
		return 0, 0, &usageError{"--shard 的 COUNT 必须在 1 到 64 之间"}
	}
	if index >= count {
		return 0, 0, &usageError{"--shard 的 INDEX 必须小于 COUNT"}
	}
	return index, count, nil
}

func splitInputs(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
