package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

var (
	nodes   = make(map[string]net.Conn)
	nodesMu sync.Mutex
)

func hashPassword(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

func handleConnection(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)

	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) != 3 {
		fmt.Println("Invalid handshake")
		return
	}

	kind, alias, arg := parts[0], parts[1], parts[2]

	if kind == "NODE" {
		passHash := arg
		nodesMu.Lock()
		if oldConn, ok := nodes[alias]; ok {
			oldConn.Close()
		}
		nodes[alias] = conn
		nodesMu.Unlock()
		fmt.Printf("[HUB] Node registered: %s (hash: %s...)\n", alias, passHash[:8])
		
		// Держим соединение открытым. Чтение будет выполнять io.Copy со стороны Клиента.
		select {}
	}

	if kind == "CLIENT" {
		// Для демо просто проверяем наличие ноды. 
		// В продакшене здесь должна быть сверка хеша пароля.
		
		nodesMu.Lock()
		nodeConn, ok := nodes[alias]
		nodesMu.Unlock()

		if !ok {
			fmt.Fprintf(conn, "500 Node not found\n")
			return
		}

		fmt.Fprintf(conn, "200 OK\n")
		fmt.Printf("[HUB] Client connected to Node: %s\n", alias)

		// Запускаем проксирование трафика между Клиентом и Нодой
		go func() {
			io.Copy(nodeConn, reader) // читаем из reader, не conn — bufio мог буферизировать данные
			nodeConn.Close()
			conn.Close()
			nodesMu.Lock()
			delete(nodes, alias)
			nodesMu.Unlock()
			fmt.Printf("[HUB] Connection closed for %s\n", alias)
		}()
		
		go func() {
			io.Copy(conn, nodeConn)
			conn.Close()
			nodeConn.Close()
		}()
		
		// Ждем, чтобы горутина не умерла сразу
		select {}
	}
}

func main() {
	ln, err := net.Listen("tcp", ":8000")
	if err != nil {
		fmt.Println("Error starting Hub:", err)
		return
	}
	fmt.Println("[HUB] Listening on :8000")

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleConnection(conn)
	}
}