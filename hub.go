package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net"
	"strings"
	"sync"
)

var (
	nodes     = make(map[string]net.Conn)
	passwords = make(map[string]string)
	mu        sync.Mutex
)

// hashHash создает SHA256 хэш от пароля
func hashPass(password string) string {
	h := sha256.Sum256([]byte(password))
	return hex.EncodeToString(h[:])
}

// readLine построчно читает из сокета без буфера, чтобы не "украсть" байты файлов
func readLine(c net.Conn) (string, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		_, err := c.Read(b)
		if err != nil {
			return "", err
		}
		if b[0] == '\n' {
			break
		}
		buf = append(buf, b[0])
	}
	return strings.TrimSpace(string(buf)), nil
}

func main() {
	listener, err := net.Listen("tcp", ":8080")
	if err != nil {
		log.Fatal("Ошибка запуска ХАБа:", err)
	}
	log.Println("ХАБ запущен на порту 8080...")

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {
	line, err := readLine(conn)
	if err != nil {
		conn.Close()
		return
	}

	parts := strings.Fields(line)
	if len(parts) < 3 {
		conn.Close()
		return
	}

	cmd, alias, pass := parts[0], parts[1], parts[2]
	hashedPass := hashPass(pass)

	mu.Lock()
	if cmd == "REGISTER" { // Подключается НОДА
		passwords[alias] = hashedPass
		nodes[alias] = conn
		log.Printf("НОДА [%s] зарегистрирована и ожидает.", alias)
		mu.Unlock()
		// Соединение остается открытым, пока не придет клиент

	} else if cmd == "CONNECT" { // Подключается КЛИЕНТ
		defer mu.Unlock()
		// Проверяем пароль
		if passwords[alias] != hashedPass {
			conn.Write([]byte("ERROR: Неверный пароль\n"))
			conn.Close()
			return
		}

		// Ищем активную ноду
		nodeConn, ok := nodes[alias]
		if !ok {
			conn.Write([]byte("ERROR: Нода оффлайн\n"))
			conn.Close()
			return
		}

		conn.Write([]byte("OK\n"))
		
		// Забираем ноду из пула свободных, так как она сейчас будет занята
		delete(nodes, alias)
		log.Printf("КЛИЕНТ подключился к НОДЕ [%s]. Проксируем...", alias)

		// Мост: перекидываем байты между клиентом и нодой
		go func() {
			io.Copy(nodeConn, conn)
			nodeConn.Close()
		}()
		io.Copy(conn, nodeConn)
		conn.Close()
	} else {
		mu.Unlock()
		conn.Close()
	}
}