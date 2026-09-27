// An authenticated Cloud Run service that publishes only scheduler identities.
package main

import (
	"context"
	"github.com/Judgment-Pack/judgment-pack-runner/internal/cloudgoogle"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	job, topic := os.Getenv("SCHEDULER_JOB"), os.Getenv("PUBSUB_TOPIC")
	if !cloudgoogle.Job.MatchString(job) || !cloudgoogle.Topic.MatchString(topic) {
		log.Fatal("scheduler job and topic are required")
	}
	client, e := cloudgoogle.Default(context.Background())
	if e != nil {
		log.Fatal("cloud service identity unavailable")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{Addr: ":" + port, Handler: cloudgoogle.Relay(job, func(ctx context.Context, s cloudgoogle.Signal) error { return client.Publish(ctx, topic, s) }), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Fatal(server.ListenAndServe())
}
