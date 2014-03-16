package main

import (
	"bytes"
	"flag"
	"fmt"
	"github.com/coreos/nova-agent-watcher/third_party/code.google.com/p/go.exp/fsnotify"
	"github.com/coreos/nova-agent-watcher/third_party/github.com/coreos/coreos-cloudinit/cloudinit"
	"github.com/coreos/nova-agent-watcher/third_party/github.com/coreos/go-systemd/dbus"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var fileHandlers = map[string]func(string, string) (*cloudinit.CloudConfig, error){
	"/etc/conf.d/net":            handleNet,
	"/root/.ssh/authorized_keys": handleSSH,
	"/etc/shadow":                handleShadow,
	"/etc/conf.d/hostname":       handleHostname,
	//	"/var/lib/heat-cfntools/cfn-userdata": handleHeatUserData,
}

func main() {
	var watch_dir = flag.String("watch-dir", ".", "Path to watch")
	var scripts_dir = flag.String("scripts-dir", "./scripts", "Path for supporting shell scripts")
	flag.Parse()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	done := make(chan bool)

	// Process events
	go func() {
		for {
			select {
			case ev := <-watcher.Event:
				log.Println("got event", ev)
				if !ev.IsCreate() {
					continue
				}
				err := runEvent(ev.Name, *watch_dir, *scripts_dir)
				if err != nil {
					log.Println("error handling event:", err)
				}
			case err := <-watcher.Error:
				log.Println("error:", err)
				done <- true
			}
		}
	}()

	for k, _ := range fileHandlers {
		full_path := filepath.Join(*watch_dir, k)
		dir_path := filepath.Dir(full_path)
		err = watcher.Watch(dir_path)
		if err != nil {
			log.Println("warn: error setting up watcher (dir doesn't exist?):", err)
		}
		err = runEvent(full_path, *watch_dir, *scripts_dir)
		if err != nil {
			log.Println("warn: initalizing event failed:", err)
		}
	}

	<-done
	watcher.Close()
}

func runEvent(full_path string, watch_dir string, scripts_dir string) error {
	if _, err := os.Stat(full_path); err != nil {
		return err
	}
	file_name, err := filepath.Rel(watch_dir, full_path)
	if err != nil {
		log.Println("error getting relative path for:", full_path)
		return err
	}
	func_key := filepath.Join("/", file_name)
	if err != nil {
		log.Println("error getting joining path for:", full_path)
		return err
	}
	if _, ok := fileHandlers[func_key]; !ok {
		log.Println("no handler found for", func_key)
		return nil
	}
	contents, err := ioutil.ReadFile(full_path)
	if err != nil {
		log.Println("error reading file", err)
		return err
	}
	config, err := fileHandlers[func_key](string(contents), scripts_dir)
	if err != nil {
		log.Println("error in handler", err)
		return err
	}
	err = runConfig(config)
	return err
}

func runConfig(config *cloudinit.CloudConfig) error {
	f, err := ioutil.TempFile("", "rackspace-cloudinit-")
	if err != nil {
		return err
	}
	log.Println("writing to:", f.Name())
	_, err = f.WriteString(config.String())
	if err != nil {
		return err
	}
	// systemd-run coreos-cloudinit --file f.Name()
	props := []dbus.Property{
		dbus.PropDescription("Unit generated and executed by coreos-cloudinit on behalf of user"),
		dbus.PropExecStart([]string{"/usr/bin/coreos-cloudinit", "--from-file", f.Name()}, false),
	}

	tmp_file := filepath.Base(f.Name())
	name := fmt.Sprintf("%s.service", tmp_file)
	log.Printf("Creating transient systemd unit '%s'", name)

	conn, err := dbus.New()
	if err != nil {
		return err
	}
	_, err = conn.StartTransientUnit(name, "replace", props...)
	return err
}

func handleNet(contents string, scripts_dir string) (*cloudinit.CloudConfig, error) {
	network_str := contents

	re := regexp.MustCompile("eth[\\d]+")
	eths := re.FindAllString(network_str, -1)

	config := cloudinit.CloudConfig{}

	configured_eths := map[string]bool{}
	for _, eth := range eths {
		// hack to prevent multiple regex matches from creating multiple files
		if configured_eths[eth] {
			continue
		}

		script := filepath.Join(scripts_dir, "gentoo-to-networkd")
		// XXX
		c1 := exec.Command("echo", contents)
		c2 := exec.Command(script, eth)

		r, w := io.Pipe()
		c1.Stdout = w
		c2.Stdin = r

		var b2 bytes.Buffer
		c2.Stdout = &b2
		err := c1.Start()
		if err != nil {
			log.Println("error: echo failed", err)
			return nil, err
		}
		err = c2.Start()
		if err != nil {
			log.Println("error: script failed", err)
			return nil, err
		}
		err = c1.Wait()
		if err != nil {
			log.Println("error: echo wait failed", err)
			return nil, err
		}
		err = w.Close()
		if err != nil {
			log.Println("error: closing pipe failed", err)
			return nil, err
		}
		err = c2.Wait()
		if err != nil {
			log.Println("error: script wait failed", err)
			return nil, err
		}
		unit := fmt.Sprintf("50-%s.network", eth)
		out := b2.String()
		u := cloudinit.Unit{
			Name:    unit,
			Content: out,
		}
		config.Coreos.Units = append(config.Coreos.Units, u)
		configured_eths[eth] = true
	}
	return &config, nil
}

// config.SSHAuthorizedKeys sets the "core" user, the other sets the root
func setKey(config *cloudinit.CloudConfig, key string) *cloudinit.CloudConfig {
	config.SSHAuthorizedKeys = append(config.SSHAuthorizedKeys, key)
	// set the password for both users
	if len(config.Users) == 0 {
		root := cloudinit.User{
			Name: "root",
		}
		root.SSHAuthorizedKeys = append(root.SSHAuthorizedKeys, key)
		config.Users = append(config.Users, root)
	} else {
		config.Users[0].SSHAuthorizedKeys = append(config.Users[0].SSHAuthorizedKeys, key)
	}
	return config
}
func handleSSH(contents string, scripts_dir string) (*cloudinit.CloudConfig, error) {
	config := cloudinit.CloudConfig{}

	ssh_keys := contents

	re := regexp.MustCompile("ssh-.+\n")
	keys := re.FindAllString(ssh_keys, -1)
	for _, key := range keys {
		key = strings.TrimRight(key, "\n")
		setKey(&config, key)
	}

	re = regexp.MustCompile("ssh-.+\\z")
	keys = re.FindAllString(ssh_keys, -1)
	for _, key := range keys {
		setKey(&config, key)
	}

	return &config, nil
}
func handleShadow(contents string, scripts_dir string) (*cloudinit.CloudConfig, error) {
	config := cloudinit.CloudConfig{}
	passwd := contents

	// root:$1$NyBnu0Gl$GBoj9u6lx3R8nyqHuxPwz/:15839:0:::::
	re := regexp.MustCompile("root:([^:]+):.+\n")
	keys := re.FindStringSubmatch(passwd)
	if len(keys) == 2 {
		passwd_hash := keys[1]

		// set the password for both users
		root := cloudinit.User{
			Name:         "root",
			PasswordHash: passwd_hash,
		}
		config.Users = append(config.Users, root)
		core := cloudinit.User{
			Name:         "core",
			PasswordHash: passwd_hash,
		}
		config.Users = append(config.Users, core)
	}
	return &config, nil
}
func handleHostname(contents string, scripts_dir string) (*cloudinit.CloudConfig, error) {
	config := cloudinit.CloudConfig{}
	hostname := contents
	// HOSTNAME="polvi-test"
	re := regexp.MustCompile("HOSTNAME=\"(.+)\"")
	keys := re.FindStringSubmatch(hostname)
	if len(keys) == 2 {
		hostname := keys[1]

		config.Hostname = hostname
	}

	return &config, nil
}
