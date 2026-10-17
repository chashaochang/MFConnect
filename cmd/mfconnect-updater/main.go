package main

import (
	"context"
	"flag"
	"log"
	"mfconnect/internal/updates"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	config := flag.String("config", "", "root-owned cloud updater configuration")
	plugin := flag.Bool("plugin", false, "run the Unraid plugin updater")
	flag.Parse()
	c := updates.WorkerConfig{}
	if *plugin {
		c = updates.WorkerConfig{Component: "plugin", ControlDir: "/boot/config/plugins/mfconnect", RuntimeDir: "/var/run/mfconnect"}
	} else if e := updates.Read(*config, &c); e != nil {
		log.Fatal("cannot read updater configuration")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := (&updates.Worker{Config: c}).Run(ctx); e != nil {
		log.Fatal(e)
	}
}
