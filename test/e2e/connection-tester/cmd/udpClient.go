/*
Copyright 2024 david amick.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// udpClientCmd represents the UDP client command
var udpClientCmd = &cobra.Command{
	Use:   "udpClient",
	Short: "Send a UDP message and ensure the message was echoed back",
	Run: func(cmd *cobra.Command, args []string) {
		serverAddressStr, err := cmd.Flags().GetString("serverAddress")
		if err != nil {
			fmt.Println("Error reading server address flag:", err)
			os.Exit(1)
		}
		if serverAddressStr == "" {
			fmt.Println("Please provide a server address")
			os.Exit(1)
		}

		message, err := cmd.Flags().GetString("message")
		if err != nil {
			fmt.Println("Error reading message flag:", err)
			os.Exit(1)
		}

		fmt.Println("Resolving server address:", serverAddressStr)
		serverAddress, err := net.ResolveUDPAddr("udp", serverAddressStr)
		if err != nil {
			fmt.Println("Error resolving server address:", err)
			os.Exit(1)
		}

		fmt.Println("Dialing UDP server at", serverAddress)
		conn, err := net.DialUDP("udp", nil, serverAddress)
		if err != nil {
			fmt.Println("Error dialing UDP:", err)
			os.Exit(1)
		}
		defer conn.Close()

		fmt.Println("Sending message to server:", message)
		_, err = conn.Write([]byte(message))
		if err != nil {
			fmt.Println("Error sending message:", err)
			os.Exit(1)
		}

		fmt.Println("Waiting for response from server")
		buffer := make([]byte, 1024)
		err = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err != nil {
			fmt.Println("Error setting read deadline:", err)
			os.Exit(1)
		}
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			fmt.Println("Error reading response:", err)
			os.Exit(1)
		}

		returnedMessage := string(buffer[:n])
		fmt.Println("Received from server:", returnedMessage)

		if returnedMessage != message {
			fmt.Println("Sent and received messages do not match")
			os.Exit(1)
		}

		fmt.Println("Sent and received messages match")
	},
}

func init() {
	rootCmd.AddCommand(udpClientCmd)

	udpClientCmd.Flags().StringP("serverAddress", "s", "localhost:8080", "Server address")
	udpClientCmd.Flags().StringP("message", "m", "Hello UDP!", "Message to send")
}
