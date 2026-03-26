package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Использование: go run node.go <hub_address> <alias> <password> <folder>")
		fmt.Println("Пример: go run node.go localhost:8080 mynode 12345 ./shared")
		return
	}

	hubAddr, alias, pass, folder := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	// Создаем папку, если ее нет
	os.MkdirAll(folder, 0755)

	log.Printf("НОДА [%s] запускается. Папка: %s", alias, folder)

	// Бесконечный цикл: обслужили клиента -> переподключились к хабу
	for {
		conn, err := net.Dial("tcp", hubAddr)
		if err != nil {
			log.Println("Не удалось подключиться к ХАБу. Повтор через 2 секунды...")
			continue
		}

		// Регистрация
		fmt.Fprintf(conn, "REGISTER %s %s\n", alias, pass)

		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			continue
		}

		parts := strings.Fields(line)
		if len(parts) == 0 {
			conn.Close()
			continue
		}

		cmd := parts[0]
		if cmd == "UPLOAD" && len(parts) == 3 {
			filename := parts[1]
			size, _ := strconv.ParseInt(parts[2], 10, 64)

			fmt.Fprint(conn, "READY\n") // Даем клиенту отмашку слать байты

			outFile, err := os.Create(filepath.Join(folder, filename))
			if err == nil {
				io.CopyN(outFile, reader, size)
				outFile.Close()
				log.Printf("Файл %s успешно получен.", filename)
			}
		} else if cmd == "DOWNLOAD" && len(parts) == 2 {
			filename := parts[1]
			inFile, err := os.Open(filepath.Join(folder, filename))
			if err != nil {
				fmt.Fprint(conn, "ERROR\n")
			} else {
				info, _ := inFile.Stat()
				fmt.Fprintf(conn, "SIZE %d\n", info.Size()) // Сообщаем размер
				io.Copy(conn, inFile)
				inFile.Close()
				log.Printf("Файл %s успешно отправлен.", filename)
			}
		}
		
		conn.Close() // Закрываем сессию с клиентом и идем на новый круг
	}
}