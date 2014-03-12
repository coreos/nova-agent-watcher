package main

import (
	"code.google.com/p/go.exp/fsnotify"
	"flag"
	"log"
	"os/exec"
	"path/filepath"
)

func main() {
	var watch_dir = flag.String("watch-dir", ".", "Path to watch")
	flag.Parse()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	done := make(chan bool)

	fileHandlers := map[string]func(string){
		"/etc/conf.d/net": handleNet,
	}
	log.Println(fileHandlers)

	// Process events
	go func() {
		for {
			select {
			case ev := <-watcher.Event:
				// the ReadFile causes an atrib event
				if !(ev.IsCreate() || (ev.IsModify() && !ev.IsAttrib())) {
					continue
				}
				file_name, err := filepath.Rel(*watch_dir, ev.Name)
				if err != nil {
					log.Println("error getting relative path for:", ev.Name)
					continue
				}
				func_key := filepath.Join("/", file_name)
				if err != nil {
					log.Println("error getting joining path for:", ev.Name)
					continue
				}
				log.Println("handling", ev)
				fileHandlers[func_key](ev.Name)
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
			log.Println("warn: not watching because directory does not exist:", dir_path)
		}
	}

	<-done
	watcher.Close()
}

func handleNet(file_path string) {
	cmd := exec.Command("./scripts/gentoo-to-networkd", "eth0", file_path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Println("error: not good output", err)
	}
	log.Printf("Command finished with stdout: %v", string(out))
	log.Println("handling:", file_path)
}
