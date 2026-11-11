// Command face-api-trigger fires one MQTT trigger at the face API and prints
// the result payload as JSON.
//
// It exists for the sample-collection workflow: the API's audit log is the
// authoritative record of a burst, but firing the trigger needs an MQTT
// client, and the paho client is already a dependency of this project.
//
// Usage: face-api-trigger <broker> <base-topic>
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: face-api-trigger <broker> <base-topic>")
		os.Exit(2)
	}
	broker, topic := os.Args[1], os.Args[2]

	opts := mqtt.NewClientOptions().AddBroker(broker).
		SetClientID("face-api-trigger").SetCleanSession(true).
		SetConnectTimeout(10 * time.Second)
	c := mqtt.NewClient(opts)
	if tok := c.Connect(); tok.Wait() && tok.Error() != nil {
		fmt.Fprintln(os.Stderr, "connect:", tok.Error())
		os.Exit(1)
	}
	defer c.Disconnect(250)

	results := make(chan []byte, 8)
	if tok := c.Subscribe(topic+"/result", 1, func(_ mqtt.Client, m mqtt.Message) {
		select {
		case results <- append([]byte(nil), m.Payload()...):
		default:
		}
	}); tok.Wait() && tok.Error() != nil {
		fmt.Fprintln(os.Stderr, "subscribe:", tok.Error())
		os.Exit(1)
	}

	// The bridge publishes the result retained, so a fresh subscriber
	// receives the previous burst's result. Drain it before triggering.
	time.Sleep(1500 * time.Millisecond)
	for {
		select {
		case <-results:
			continue
		default:
		}
		break
	}

	if tok := c.Publish(topic+"/trigger", 1, false, "on"); tok.Wait() && tok.Error() != nil {
		fmt.Fprintln(os.Stderr, "publish:", tok.Error())
		os.Exit(1)
	}

	select {
	case payload := <-results:
		// Reformat so the output is a single line of JSON.
		var v any
		if err := json.Unmarshal(payload, &v); err != nil {
			fmt.Println(string(payload))
			return
		}
		out, err := json.Marshal(v)
		if err != nil {
			fmt.Println(string(payload))
			return
		}
		fmt.Println(string(out))
	case <-time.After(40 * time.Second):
		fmt.Fprintln(os.Stderr, "timeout: no result within 40s")
		os.Exit(2)
	}
}
