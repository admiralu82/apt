package main

import (
	"fmt"

	"./xtask"

	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

var aptExe = "apt.exe"
var apthExe = "apth.exe"

var logger chan []interface{} = make(chan []interface{}, 100)

func Logger(a ...interface{}) {
	logger <- a
}

func main() {
	// настройка логирования
	//настройка логирования
	go func() {
		f_log, err := os.OpenFile("apth.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			fmt.Println("error opening log file: %v", err)
		}
		log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
		defer f_log.Close()
		wrt := io.MultiWriter(f_log, os.Stdout)
		log.SetOutput(wrt)
		Logger("Старт логгирования.")
		for {
			select {
			case s := <-logger:
				log.Println(s)
			}
		}
	}()

	// проверка что мы одни
	windowsProcesses, e := xtask.Processes()
	if e != nil {
		Logger("error Processes", e)
	} else {
		lastCount := 0
		for _, v := range windowsProcesses {
			if strings.Compare(strings.ToLower(v.Exe), apthExe) == 0 {
				lastCount++
			}
		}
		if lastCount == 2 {
			fmt.Println("Одна копия уже запущена.")
			return
		}
	}

	lastScan := true
	lastCount := 0
	for {
		time.Sleep(15 * time.Second)
		// проверяем запущен ли apt.exe
		windowsProcesses, e := xtask.Processes()
		if e != nil {
			Logger("error Processes", e)
			continue
		}

		lastCount = 0
		for _, v := range windowsProcesses {
			if strings.Compare(strings.ToLower(v.Exe), aptExe) == 0 {
				lastCount++
			}
		}

		if lastScan == true {
			if lastCount == 0 {
				Logger("Apt.exe не запущен. Подождем немоного.")
				lastScan = false
				continue
			}

			if lastCount == 1 {
				// нормальная работа программы - е
				_, min, _ := time.Now().Clock()
				if min%5 == 0 {
					Logger("Apt.exe запущен. Все нормально.")
				}

				continue
			}

			if lastCount > 1 {
				// две запущенных копии, подождем немоного
				Logger("Apt.exe запущен ", lastCount, " раз. Подождем немоного.")
				lastScan = false
			}
		}

		if lastScan == false {
			if lastCount == 1 {
				Logger("Apt.exe не был запущен в прошлый раз. Но запустился.")
				lastScan = true
				continue
			}

			if lastCount > 1 {
				// завершаем работу всех apt.exe
				//xtask.KillAll(aptExe)
				for _, v := range windowsProcesses {
					if strings.Compare(strings.ToLower(v.Exe), aptExe) == 0 {
						proc, err := os.FindProcess(v.ProcessID)
						if err != nil {
							Logger("Error Find Process.", err)
							continue
						}
						proc.Kill()

						Logger("Несколько копий. Kill process", v.Exe, v.ProcessID)
						time.Sleep(1 * time.Second)
					}
				}

				// подождем немоного
				lastScan = false
				continue
			}

			if lastCount == 0 {
				//проверяем наличие apt.exe
				_, err := os.Stat(aptExe)
				if err == nil {
					// файл существует
					//запускаем apt.exe
					Logger("Apt.exe не запущен. Запукаем.")

					cmd := exec.Command(aptExe)
					cmd.Start()
					lastScan = true
				}
				continue
			}
		}
	}
}
