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

	"github.com/spf13/cobra"
)

// udpServerCmd represents the UDP server command
var udpServerCmd = &cobra.Command{
	Use:   "udpServer",
	Short: "Run an echo server on UDP, for testing",
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

		listenAddress, err := net.ResolveUDPAddr("udp", listenAddressStr)
		if err != nil {
			fmt.Println("Error resolving listen address:", err)
			os.Exit(1)
		}

		conn, err := net.ListenUDP("udp", listenAddress)
		if err != nil {
			fmt.Println("Error listening on UDP:", err)
			os.Exit(1)
		}
		defer conn.Close()

		fmt.Println("UDP Echo Server listening on:", listenAddress)

		buffer := make([]byte, 1024)
		for {
			// Read data from the client
			n, clientAddr, err := conn.ReadFromUDP(buffer)
			if err != nil {
				fmt.Println("Error reading from UDP:", err)
				continue
			}

			// Echo the data back to the client
			fmt.Println("Received from:", clientAddr, ":", string(buffer[:n]))
			_, err = conn.WriteToUDP(buffer[:n], clientAddr)
			if err != nil {
				fmt.Println("Error writing to UDP:", err)
			}
		}
	},
}

func init() {
	rootCmd.AddCommand(udpServerCmd)

	udpServerCmd.Flags().StringP("listenAddress", "l", ":8080", "Listen address")
}
