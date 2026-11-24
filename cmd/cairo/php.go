package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cairophp/cairo/internal/core"
)

const phpUsage = `usage:
  cairo php [options] [-f] <file> [--] [args...]
  cairo php [options] -r <code> [--] [args...]
  cairo php [options] -- [args...]        (the script is read from standard input)

  -c <path>|<file>   look for php.ini in this directory, or use this file
  -n                 use no php.ini
  -d foo[=bar]       set the INI entry foo to bar
  -f <file>          run the file
  -r <code>          run the code, without <?php tags
  -l                 check the syntax of the files only
  -i                 PHP information
  -m                 the compiled-in modules
  -v                 the version
  --ini              the php.ini files in use
  --ri <name>        the configuration of the extension <name>
  -h                 this help

The script runs under Cairo's fiber scheduler. CAIRO_SCHEDULER=off in the
environment runs it as PHP does, for a program that has an event loop of its
own for fibers. CAIRO_PHP_CLI=/path/to/php hands the command to that PHP
instead of running it in this binary.`

// What an option that prints information runs: PHP code, which reads what
// it needs to be told from $argv.
const (
	phpVersionCode = `printf("PHP %s (%s) (%s)\nCopyright (c) The PHP Group\nZend Engine v%s, Copyright (c) Zend Technologies\n    with Cairo %s\n", PHP_VERSION, PHP_SAPI, PHP_ZTS ? 'ZTS' : 'NTS', zend_version(), $argv[1]);`
	phpModulesCode = `$m = get_loaded_extensions(); natcasesort($m); $z = get_loaded_extensions(true); natcasesort($z);
echo "[PHP Modules]\n", implode("\n", $m), "\n\n[Zend Modules]\n", implode("\n", $z), $z ? "\n\n" : "\n";`
	phpIniCode = `printf("Configuration File (php.ini) Path: \"%s\"\nLoaded Configuration File:         %s\nAdditional .ini files parsed:      %s\n",
    get_cfg_var('cfg_file_path') === false ? PHP_CONFIG_FILE_PATH : dirname(get_cfg_var('cfg_file_path')),
    php_ini_loaded_file() ?: '(none)', trim((string) php_ini_scanned_files()) ?: '(none)');`
	phpExtensionCode = `if (!extension_loaded($argv[1])) { fwrite(STDERR, "Extension '{$argv[1]}' not present.\n"); exit(1); }
(new ReflectionExtension($argv[1]))->info();`
)

// phpStdinName is what PHP calls a script that is not a file.
const phpStdinName = "Standard input code"

// phpCommand says whether argv starts PHP's command line and not Cairo's, and
// with which arguments: `cairo php ...`, cairo under the name php (a link),
// or cairo with PHP's own arguments. The last is how PHP code starts PHP
// again: PHP_BINARY is this binary, and `PHP_BINARY artisan queue:work` or
// `PHP_BINARY -d memory_limit=-1 script.php` must run the script.
func phpCommand(argv []string) ([]string, bool) {
	if len(argv) == 0 {
		return nil, false
	}
	if filepath.Base(argv[0]) == "php" {
		return argv[1:], true
	}
	if len(argv) < 2 {
		return nil, false
	}
	switch first := argv[1]; {
	case first == "php":
		return argv[2:], true
	case first == "serve", first == "gen", first == "version", first == "help", first == "-h", first == "--help":
		return nil, false
	case strings.HasPrefix(first, "-"):
		return argv[1:], true
	default:
		info, err := os.Stat(first)
		return argv[1:], err == nil && info.Mode().IsRegular()
	}
}

// phpOption is an option of PHP's command line that Cairo takes: its long
// name and whether a value follows.
type phpOption struct {
	long  string
	value bool
}

var phpOptions = map[byte]phpOption{
	'c': {"php-ini", true},
	'n': {"no-php-ini", false},
	'd': {"define", true},
	'f': {"file", true},
	'r': {"run", true},
	'l': {"syntax-check", false},
	'i': {"info", false},
	'm': {"modules", false},
	'v': {"version", false},
	'h': {"help", false},
	'?': {"usage", false},
	'I': {"ini", false},          // long only
	'x': {"ri", true},            // long only
	'e': {"profile-info", false}, // accepted and without effect, as are the next three
	'q': {"no-header", false},
	'C': {"no-chdir", false},
	'H': {"hide-args", false},
}

// phpArguments is PHP's command line, read.
type phpArguments struct {
	run     core.CLI
	script  string // -f
	help    bool
	started bool // -r or an option that prints information was read: no script is taken from the arguments
}

// parsePHP reads the arguments as PHP's command line does: options up to the
// first argument that is none or to "--", then the script, then what the
// script gets.
func parsePHP(args []string) (core.CLI, bool, error) {
	p := phpArguments{run: core.CLI{Mode: core.CLIFile}}
	rest, dashes, err := p.options(args)
	if err != nil {
		return core.CLI{}, false, err
	}
	if p.run.INIPath == "" && !p.run.NoINI {
		p.run.INIPath = os.Getenv("CAIRO_PHP_INI") // the php.ini of `cairo serve`
	}
	p.run.NoScheduler = os.Getenv("CAIRO_SCHEDULER") == "off"
	switch {
	case p.run.Mode == core.CLILint:
		if p.script != "" {
			rest = append([]string{p.script}, rest...)
		}
		p.run.Args = rest
	case p.started:
		p.run.Args = append(p.run.Args, rest...)
	case p.script != "":
		p.run.Args = append([]string{p.script}, rest...)
	case len(rest) > 0 && !dashes:
		p.run.Args = rest
	default:
		p.run.Mode = core.CLIStdin
		p.run.Args = append([]string{phpStdinName}, rest...)
	}
	return p.run, p.help, nil
}

// options reads the options at the start of args. It returns the arguments
// after them and whether "--" ended them.
func (p *phpArguments) options(args []string) ([]string, bool, error) {
	for len(args) > 0 {
		arg := args[0]
		if arg == "--" {
			return args[1:], true, nil
		}
		if len(arg) < 2 || arg[0] != '-' {
			break
		}
		args = args[1:]
		if strings.HasPrefix(arg, "--") {
			name, value, attached := strings.Cut(arg[2:], "=")
			short, ok := phpLong(name)
			if !ok {
				return nil, false, fmt.Errorf("--%s is not an option of cairo php", name)
			}
			if phpOptions[short].value && !attached {
				if len(args) == 0 {
					return nil, false, fmt.Errorf("--%s needs a value", name)
				}
				value, args = args[0], args[1:]
			}
			p.set(short, value)
			continue
		}
		// Short options: several in one argument, a value attached or next.
		for i := 1; i < len(arg); i++ {
			option, ok := phpOptions[arg[i]]
			if !ok || arg[i] == 'I' || arg[i] == 'x' {
				return nil, false, fmt.Errorf("-%c is not an option of cairo php", arg[i])
			}
			if !option.value {
				p.set(arg[i], "")
				continue
			}
			value := arg[i+1:]
			if value == "" {
				if len(args) == 0 {
					return nil, false, fmt.Errorf("-%c needs a value", arg[i])
				}
				value, args = args[0], args[1:]
			}
			p.set(arg[i], value)
			break
		}
	}
	return args, false, nil
}

func phpLong(name string) (byte, bool) {
	for short, option := range phpOptions {
		if option.long == name {
			return short, true
		}
	}
	return 0, false
}

// set applies one option. The first of -r and the options that print
// information decides what runs, as with PHP.
func (p *phpArguments) set(option byte, value string) {
	code := func(code string, args ...string) {
		if !p.started {
			p.started = true
			p.run.Mode, p.run.Code = core.CLICode, code
			p.run.Args = append([]string{phpStdinName}, args...)
		}
	}
	switch option {
	case 'c':
		p.run.INIPath = value
	case 'n':
		p.run.NoINI = true
	case 'd':
		p.run.INI += phpDefine(value)
	case 'f':
		p.script = value
	case 'r':
		code(value)
	case 'l':
		p.run.Mode = core.CLILint
	case 'i':
		code("phpinfo();")
	case 'm':
		code(phpModulesCode)
	case 'v':
		code(phpVersionCode, version)
	case 'I':
		code(phpIniCode)
	case 'x':
		code(phpExtensionCode, value)
	case 'h', '?':
		p.help = true
	}
}

// phpDefine is the php.ini line of a -d option, as PHP writes it: "foo" turns
// foo on, and a value that starts with neither a letter, a digit nor a quote
// is quoted.
func phpDefine(arg string) string {
	name, value, found := strings.Cut(arg, "=")
	if !found {
		return name + "=1\n"
	}
	if value == "" || value[0] == '"' || value[0] == '\'' || isAlnum(value[0]) {
		return arg + "\n"
	}
	return name + `="` + value + "\"\n"
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// php runs PHP's command line and returns the status the process ends with.
func php(args []string) int {
	if status, handed := handToOtherPHP(args); handed {
		return status
	}
	run, help, err := parsePHP(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairo php:", err)
		fmt.Fprintln(os.Stderr, phpUsage)
		return 1
	}
	if help {
		fmt.Println(phpUsage)
		return 0
	}
	run.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	status, err := core.RunCLI(run)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cairo php:", err)
		return 1
	}
	return status
}
