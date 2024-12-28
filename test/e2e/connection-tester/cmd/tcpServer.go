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
	"io"
	"net"
	"os"

	"github.com/spf13/cobra"
)

// tcpServerCmd represents the TCP server command
var tcpServerCmd = &cobra.Command{
	Use:   "tcpServer",
	Short: "Run an echo server on TCP, for testing",
	Run: func(cmd *cobra.Command, args []string) {
		listenAddressStr, err := cmd.Flags().GetString("listenAddress")
		if err != nil {
			fmt.Println("Error reading listen address flag:", err)
			os.Exit(1)
		}
		if listenAddressStr == "" {
			fmt.Println("Please provide a listen address")
			os.Exit(1)
		}

		tcpListener, err := net.Listen("tcp", listenAddressStr)
		if err != nil {
			fmt.Println("Error listening:", err)
			os.Exit(1)
		}
		defer tcpListener.Close()

		fmt.Println("TCP Echo Server listening on:", listenAddressStr)

		for {
			tcpConn, err := tcpListener.Accept()
			if err != nil {
				fmt.Println("Error accepting connection:", err)
				continue
			}

			go handleConnection(tcpConn)
		}
	},
}

func handleConnection(conn net.Conn) {
	defer conn.Close()

	buffer := make([]byte, 1024)
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			if err != io.EOF {
				fmt.Println("Error reading:", err)
			}
			break
		}

		_, err = conn.Write(buffer[:n])
		if err != nil {
			fmt.Println("Error writing:", err)
			break
		}
	}
}

func init() {
	rootCmd.AddCommand(tcpServerCmd)

	tcpServerCmd.Flags().StringP("listenAddress", "l", ":8080", "Listen address")
}
