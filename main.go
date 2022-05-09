package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"golang.design/x/hotkey"
	"golang.design/x/hotkey/mainthread"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

type (
	CommandLine struct {
		Command     string        `json:"command"`
		Args        []string      `json:"args,omitempty"`
		IgnoreError bool          `json:"ignore_error,omitempty"`
		Background  bool          `json:"background,omitempty"`
		NullStdout  bool          `json:"null_stdout,omitempty"`
		Timeout     time.Duration `json:"timeout,omitempty"`
	}

	Config struct {
		Startup  []CommandLine `json:"startup"`
		Cleanup  []CommandLine `json:"cleanup"`
		ShowApps []string      `json:"show_apps"`
		HideApps []string      `json:"hide_apps"`
	}

	HotKey struct {
		Name   string
		Handle *hotkey.Hotkey
	}
)

var (
	listenKeys   []*HotKey
	globalCfg    *Config
	cleanupMutex sync.Mutex
)

func main() {
	err := loadConfig()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	handleInterrupt()

	err = regHotKeys()
	if err != nil {
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("[RUN STARTUP COMMANDS]")
	err = runCommands(globalCfg.Startup, false)
	if err != nil {
		cleanup()
		os.Exit(1)
	} else {
		mainthread.Init(listenHotKeys)
	}
}

func cleanup() {
	if cleanupMutex.TryLock() {
		fmt.Println()
		fmt.Println("[RUN CLEANUP COMMANDS]")
		runCommands(globalCfg.Cleanup, true)
		cleanupMutex.Unlock()
	}
}

func handleInterrupt() {
	wg := sync.WaitGroup{}
	wg.Add(1)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		wg.Done()
		<-c
		cleanup()
		os.Exit(1)
	}()
}

func loadConfig() error {
	var wd string

	if len(os.Args) > 1 {
		wd = os.Args[1] + string(os.PathSeparator)
	}
	dir, _ := filepath.Abs(filepath.Dir(wd))

	f, err := os.Open(filepath.Join(dir, "commands.json"))
	if err != nil {
		return err
	}
	defer f.Close()

	b, err := ioutil.ReadAll(f)
	if err != nil {
		return err
	}

	globalCfg = &Config{}
	err = json.Unmarshal(b, globalCfg)
	if err != nil {
		return err
	}

	return nil
}

func regHotKeys() error {
	err := regHotKey("CTL + SHIFT + ALT + K", hotkey.KeyK, hotkey.ModCtrl, hotkey.ModShift, hotkey.ModAlt)
	if err != nil {
		return err
	}
	return nil
}

func regHotKey(name string, key hotkey.Key, mods ...hotkey.Modifier) error {
	ms := []hotkey.Modifier{}
	ms = append(ms, mods...)
	hk := hotkey.New(ms, key)

	err := hk.Register()
	if err != nil {
		fmt.Printf("ERR: register hotkey %s failed, %s\n", name, err)
		return err
	}

	fmt.Printf("[REGISTER HOTKEY] %s ok\n", name)
	listenKeys = append(listenKeys, &HotKey{Name: name, Handle: hk})
	return nil
}

func listenHotKeys() {
	cases := make([]reflect.SelectCase, len(listenKeys))
	for i, reg := range listenKeys {
		cases[i] = reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(reg.Handle.Keydown()),
		}
	}

	for {
		chosen, _, ok := reflect.Select(cases)
		if !ok {
			break
		}

		switch chosen {
		case 0:
			fmt.Println("[RUN CLEANUP COMMANDS]")
			runCommands(globalCfg.Cleanup, true)
			os.Exit(0)
		}
	}
}

func runCommands(commands []CommandLine, ignoreErrors bool) error {
	for _, cli := range commands {
		err := runCommand(cli)
		if err != nil {
			fmt.Printf("---> %s\n", err)
			if !ignoreErrors {
				return err
			}
		}
	}
	return nil
}

func runCommand(cli CommandLine) error {
	if cli.Command[0] == '!' {
		return runMacro(cli)
	}

	fmt.Printf("run: %s %s\n", cli.Command, strings.Join(cli.Args, " "))
	cmd := exec.Command(cli.Command, cli.Args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	err = cmd.Start()
	if err != nil {
		return err
	}

	if cli.Background {
		return nil
	}

	out, _ := ioutil.ReadAll(stdout)
	errout, _ := ioutil.ReadAll(stderr)

	b := new(bytes.Buffer)
	wr := transform.NewWriter(b, unicode.UTF8.NewDecoder())
	wr.Write(out)
	wr.Write(errout)
	wr.Close()
	fmt.Println(b.String())
	return nil
}

func runMacro(cli CommandLine) error {
	fmt.Printf("run macro: %s %s\n", cli.Command, strings.Join(cli.Args, " "))
	switch strings.ToUpper(cli.Command) {
	case "!WAIT_FILE":
		return runMacroWaitFile(cli)
	case "!WAIT_PORT":
		return runMacroWaitPort(cli)
	default:
		return fmt.Errorf("unknown macro %s", cli.Command)
	}
}

func runMacroWaitFile(cli CommandLine) error {
	expire := time.Now().Add(cli.Timeout * time.Second).UnixMilli()
	for {
		ok := true
		for _, name := range cli.Args {
			_, err := os.Stat(name)
			if err != nil {
				ok = false
				break
			}
		}

		if ok {
			return nil
		}

		if time.Now().UnixMilli() >= expire {
			break
		}

		time.Sleep(time.Second / 2)
	}

	return errors.New("timeout")
}

func runMacroWaitPort(cli CommandLine) error {
	expire := time.Now().Add(cli.Timeout * time.Second).UnixMilli()
	for {
		ok := true
		for _, port := range cli.Args {
			conn, err := net.DialTimeout("tcp", port, time.Second/2)
			if err != nil {
				println(err.Error())
				ok = false
				break
			}
			if conn != nil {
				conn.Close()
			}
		}

		if ok {
			return nil
		}

		if time.Now().UnixMilli() >= expire {
			break
		}

		time.Sleep(time.Second / 2)
	}

	return errors.New("timeout")
}
