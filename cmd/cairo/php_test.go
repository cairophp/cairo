package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cairophp/cairo/internal/core"
)

func TestPHPCommand(t *testing.T) {
	file := filepath.Join(t.TempDir(), "artisan")
	if err := os.WriteFile(file, []byte("<?php"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		argv []string
		args []string
		ok   bool
	}{
		"cairo php":               {[]string{"cairo", "php", "-v"}, []string{"-v"}, true},
		"cairo php alone":         {[]string{"cairo", "php"}, []string{}, true},
		"a link named php":        {[]string{"/usr/local/bin/php", "artisan", "migrate"}, []string{"artisan", "migrate"}, true},
		"an option of PHP's":      {[]string{"cairo", "-d", "a=b", "x.php"}, []string{"-d", "a=b", "x.php"}, true},
		"a file":                  {[]string{"cairo", file, "queue:work"}, []string{file, "queue:work"}, true},
		"a directory is no file":  {[]string{"cairo", filepath.Dir(file)}, nil, false},
		"serve":                   {[]string{"cairo", "serve", "server.php"}, nil, false},
		"help":                    {[]string{"cairo", "--help"}, nil, false},
		"a word that is no file":  {[]string{"cairo", "serv"}, nil, false},
		"no arguments":            {[]string{"cairo"}, nil, false},
		"a command wins a file":   {[]string{"cairo", "version"}, nil, false},
		"no program name, at all": {nil, nil, false},
	}
	for name, c := range cases {
		args, ok := phpCommand(c.argv)
		if ok != c.ok || (ok && !reflect.DeepEqual(args, c.args)) {
			t.Errorf("%s: %q, %v; want %q, %v", name, args, ok, c.args, c.ok)
		}
	}
}

func TestParsePHP(t *testing.T) {
	t.Setenv("CAIRO_PHP_INI", "")
	stdin := phpStdinName
	cases := map[string]struct {
		args []string
		want core.CLI
	}{
		"a script and its arguments": {[]string{"artisan", "migrate", "--force"}, core.CLI{Mode: core.CLIFile, Args: []string{"artisan", "migrate", "--force"}}},
		"options, then the script":   {[]string{"-d", "memory_limit=-1", "-dfoo", "-n", "a.php", "-d", "x"}, core.CLI{Mode: core.CLIFile, Args: []string{"a.php", "-d", "x"}, INI: "memory_limit=\"-1\"\nfoo=1\n", NoINI: true}},
		"-f and --":                  {[]string{"-f", "a.php", "--", "-v"}, core.CLI{Mode: core.CLIFile, Args: []string{"a.php", "-v"}}},
		"long options":               {[]string{"--define", "a=b", "--php-ini=/etc/php", "--file", "a.php"}, core.CLI{Mode: core.CLIFile, Args: []string{"a.php"}, INI: "a=b\n", INIPath: "/etc/php"}},
		"-r":                         {[]string{"-r", "echo 1;", "a", "-b"}, core.CLI{Mode: core.CLICode, Code: "echo 1;", Args: []string{stdin, "a", "-b"}}},
		"-r attached":                {[]string{"-recho 1;"}, core.CLI{Mode: core.CLICode, Code: "echo 1;", Args: []string{stdin}}},
		"the first of -r and -v":     {[]string{"-r", "echo 1;", "-v"}, core.CLI{Mode: core.CLICode, Code: "echo 1;", Args: []string{stdin}}},
		"-v":                         {[]string{"--version"}, core.CLI{Mode: core.CLICode, Code: phpVersionCode, Args: []string{stdin, version}}},
		"--ri":                       {[]string{"--ri", "curl"}, core.CLI{Mode: core.CLICode, Code: phpExtensionCode, Args: []string{stdin, "curl"}}},
		"standard input":             {nil, core.CLI{Mode: core.CLIStdin, Args: []string{stdin}}},
		"standard input, arguments":  {[]string{"-n", "--", "a"}, core.CLI{Mode: core.CLIStdin, Args: []string{stdin, "a"}, NoINI: true}},
		"a lone dash is no option":   {[]string{"-", "a"}, core.CLI{Mode: core.CLIFile, Args: []string{"-", "a"}}},
		"-l":                         {[]string{"-nl", "a.php", "b.php"}, core.CLI{Mode: core.CLILint, Args: []string{"a.php", "b.php"}, NoINI: true}},
		"-l with -f":                 {[]string{"-l", "-f", "a.php", "b.php"}, core.CLI{Mode: core.CLILint, Args: []string{"a.php", "b.php"}}},
		"options without effect":     {[]string{"-q", "-C", "a.php"}, core.CLI{Mode: core.CLIFile, Args: []string{"a.php"}}},
	}
	for name, c := range cases {
		got, help, err := parsePHP(c.args)
		if err != nil || help || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, help %v, err %v; want %+v", name, got, help, err, c.want)
		}
	}
	for _, bad := range [][]string{{"-a"}, {"-S", "localhost:8000"}, {"--server"}, {"-d"}, {"--define"}, {"-I"}, {"-x", "curl"}} {
		if _, _, err := parsePHP(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if _, help, err := parsePHP([]string{"-h"}); !help || err != nil {
		t.Errorf("-h: help %v, err %v", help, err)
	}
}

func TestParsePHPIniFromTheEnvironment(t *testing.T) {
	t.Setenv("CAIRO_PHP_INI", "/etc/cairo/php.ini")
	if got, _, _ := parsePHP([]string{"a.php"}); got.INIPath != "/etc/cairo/php.ini" {
		t.Errorf("php.ini = %q, want the one of CAIRO_PHP_INI", got.INIPath)
	}
	if got, _, _ := parsePHP([]string{"-c", "/tmp/php.ini", "a.php"}); got.INIPath != "/tmp/php.ini" {
		t.Errorf("php.ini = %q, want the one of -c", got.INIPath)
	}
	if got, _, _ := parsePHP([]string{"-n", "a.php"}); got.INIPath != "" {
		t.Errorf("php.ini = %q, want none with -n", got.INIPath)
	}
}
