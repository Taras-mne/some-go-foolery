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
	if len(os.Args) < 6 {
		fmt.Println("Использование: go run client.go <hub_address> <alias> <password> <upload|download> <filepath>")
		fmt.Println("Пример: go run client.go localhost:8080 mynode 12345 upload test.txt")
		return
	}

	hubAddr, alias, pass, cmd, filePath := os.Args[1], os.Args[2], os.Args[3], strings.ToLower(os.Args[4]), os.Args[5]
	filename := filepath.Base(filePath)

	conn, err := net.Dial("tcp", hubAddr)
	if err != nil {
		log.Fatal("Ошибка подключения к ХАБу:", err)
	}
	defer conn.Close()

	// Запрашиваем коннект к ноде
	fmt.Fprintf(conn, "CONNECT %s %s\n", alias, pass)

	reader := bufio.NewReader(conn)
	status, _ := reader.ReadString('\n')
	if strings.TrimSpace(status) != "OK" {
		log.Fatal("Ошибка от ХАБа:", status)
	}

	if cmd == "upload" {
		info, err := os.Stat(filePath)
		if err != nil {
			log.Fatal("Файл не найден:", err)
		}

		// Инициируем UPLOAD
		fmt.Fprintf(conn, "UPLOAD %s %d\n", filename, info.Size())
		
		resp, _ := reader.ReadString('\n')
		if strings.TrimSpace(resp) == "READY" {
			file, _ := os.Open(filePath)
			io.Copy(conn, file)
			file.Close()
			log.Println("Файл успешно загружен на НОДУ.")
		} else {
			log.Fatal("НОДА отказала в приеме файла.")
		}

	} else if cmd == "download" {
		// Инициируем DOWNLOAD
		fmt.Fprintf(conn, "DOWNLOAD %s\n", filename)
		
		resp, _ := reader.ReadString('\n')
		parts := strings.Fields(resp)
		if len(parts) == 2 && parts[0] == "SIZE" {
			size, _ := strconv.ParseInt(parts[1], 10, 64)
			
			file, err := os.Create(filename)
			if err != nil {
				log.Fatal("Ошибка создания файла:", err)
			}
			io.CopyN(file, reader, size)
			file.Close()
			log.Println("Файл успешно скачан.")
		} else {
			log.Fatal("Ошибка скачивания: файла нет или НОДА вернула ошибку.")
		}
	} else {
		log.Fatal("Неизвестная команда. Используйте upload или download.")
	}
}