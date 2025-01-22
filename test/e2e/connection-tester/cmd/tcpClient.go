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

// tcpClientCmd represents the TCP client command
var tcpClientCmd = &cobra.Command{
	Use:   "tcpClient",
	Short: "Send a TCP message and ensure the message was echoed back",
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

		conn, err := net.Dial("tcp", serverAddressStr)
		if err != nil {
			fmt.Println("Error connecting:", err)
			return
		}
		defer conn.Close()

		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

		_, err = conn.Write([]byte(message))
		if err != nil {
			fmt.Println("Error sending message:", err)
			return
		}

		buffer := make([]byte, 1024)
		n, err := conn.Read(buffer)
		if err != nil {
			fmt.Println("Error reading:", err)
			return
		}

		returnedMessage := string(buffer[:n])
		if returnedMessage != message {
			fmt.Println("Sent and received messages do not match")
			os.Exit(1)
		}

		fmt.Println("Sent and received messages match")
	},
}

func init() {
	rootCmd.AddCommand(tcpClientCmd)

	tcpClientCmd.Flags().StringP("serverAddress", "s", "localhost:8080", "Server address")
	tcpClientCmd.Flags().StringP("message", "m", "Hello TCP!", "Message to send")
}
