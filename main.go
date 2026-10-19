// Command mssql-plugin is an Inflowenger plugin node for Microsoft SQL Server.
//
// It exposes four actions on the workflow canvas — read a query, execute a
// write, insert or upsert a record, and create a table if it does not already
// exist — over a connection the platform ships with every call.
//
// The plugin holds no database configuration. It declares what a connection
// needs (see the settings form); the platform stores that as a named settings
// profile and ships the values with every call as `body.settings`.
package main

import (
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/mssql-plugin/internal/actions"
)

const version = "v0.1.0"

func main() {
	envFile := os.Getenv("INFLOW_ENV_FILE")
	if envFile == "" {
		envFile = ".env.inflow"
	}

	// The dotenv carries the platform identity only — PLUGIN_ID, INFRA_CRED,
	// INFRA_URL. Database credentials never live here.
	plugin, err := sdkv1.NewPlugin(sdkv1.WithDotEnv(envFile))
	if err != nil {
		log.Fatalf("mssql plugin: cannot connect to infra (%s): %v", envFile, err)
	}

	registry := actions.New()

	plugin.Intro(sdkv1.PluginIntro{
		Name:     "MSSQL",
		Author:   "Inflowenger",
		Version:  version,
		Settings: registry.SettingsForm(),
	})
	plugin.RequiredParams(registry.Settings())

	all := registry.All()
	plugin.AddAction(all...)
	plugin.AddMeta(registry.Metas()...)

	if err := plugin.Start(); err != nil {
		log.Fatalf("mssql plugin: start: %v", err)
	}

	methods := make([]string, 0, len(all))
	for _, action := range all {
		methods = append(methods, action.Method)
	}
	log.Printf("mssql plugin %s ready with %d actions: %s", version, len(all), strings.Join(methods, ", "))
	log.Printf("mssql plugin: each call brings its own connection in body.settings — bind a settings profile to the node")

	// Start() only wires up subscriptions; the process has to stay alive to
	// serve them.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("mssql plugin: shutting down")
}
