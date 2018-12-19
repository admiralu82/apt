package main

//go build -ldflags -H=windowsgui  apt.go

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"golang.org/x/text/encoding/charmap"

	yaml "gopkg.in/yaml.v2"

	"github.com/gorilla/websocket"
	"github.com/lxn/walk"
	_ "github.com/mattn/go-sqlite3"
	_ "github.com/nakagami/firebirdsql"
)

// определяет является ли сервером или клиентом
var isServer bool

//  конфигураций
var cfgMain Settings

// пакет с текущей информацией из базы и пустым пакетом действия
//var packetInfo progInfo

// определеяем детальность логов
var debug = false

//шаблоны страниц для сервера
var homeTemplate = template.Must(template.New("").Parse(""))

// база данных сервера
var dbSrv *sql.DB

// update - обновление restart - перезагрузка
var selfupdate chan string

// имя программы
var appName = filepath.Base(os.Args[0])

// канал для приема последних событий
var guiLastWork chan progInfo

// канал для приема  задач планировщика
var taskChan chan cfgTask

const (
	version = 17
	// служебные пакеты обмена
	shedSelfRestart = "SelfRestart"
	shedHello       = "Hello"
)

type Settings struct {
	Settings struct {
		DB_CONFIG    string
		SRV_CONFIG   string
		SMTP_SERVER  string
		SMTP_FROM    string
		SMTP_TO      string
		SMTP_SUBJECT string
		SMTP_PASS    string
	}
	Tasks []cfgTask
}

type taskStatus struct {
	SHED_TYPE string
	START     time.Time
	STOP      time.Time
	EXITCODE  string
	LOG       string
}

type progInfo struct {
	APT_VERSION int
	PRED_ID     string
	NODE_ID     string
	NODE_NAME   string
	EXE_VERSION map[string]string
	NODE_CFG    string
	SYSVER      string
	TASKSTATUS  taskStatus
}

type cfgTask struct {
	ShedType string
	BinExe   string
	Path     string
	WorkDir  string
	Param    []string
	Start    int
	Stop     int
	Day      int
	Interval int
}

func sqlINFO() progInfo {

	var dbString string = cfgMain.Settings.DB_CONFIG

	info := progInfo{EXE_VERSION: make(map[string]string)}

	conn, err := sql.Open("firebirdsql", dbString)
	if err != nil {
		log.Panic("Cannot connect to Firebird")
	}
	defer conn.Close()

	row := conn.QueryRow("SELECT ORGANIZATION.PRED_ID, ORGANIZATION.NODE_ID, ORGANIZATION.SYSVERNO, NODES.node_name FROM ORGANIZATION INNER JOIN NODES ON ORGANIZATION.node_id = NODES.node_id;")

	var pred_id, node_id, node_name, sysver string
	row.Scan(&pred_id, &node_id, &sysver, &node_name)

	info.PRED_ID = pred_id
	info.NODE_ID = node_id
	info.NODE_NAME = node_name
	info.APT_VERSION = version
	info.SYSVER = sysver

	rows, err := conn.Query("SELECT * FROM EXE_VERSION;")
	if err != nil {
		log.Println("SQL info error ", err)
	}
	var name, version string
	for rows.Next() {
		rows.Scan(&name, &version)

		info.EXE_VERSION[name] = version
	}

	info.TASKSTATUS = taskStatus{}

	cfg, _ := ioutil.ReadFile("apt.cfg")
	info.NODE_CFG = string(cfg)
	return info

}

func doUpdate() bool {

	var addr_srv string = cfgMain.Settings.SRV_CONFIG
	// for _, v := range cfgMain {
	// 	if v.ShedType == shedSRVCONFIG {
	// 		addr_srv = v.Param
	// 	}
	// }
	if addr_srv == "" {
		log.Panic("Автообновление: Ошибка адрес сервера не задан. Поправте конфигурацию")
	}

	u := "http://" + addr_srv + "/aptupdate?ver=" + fmt.Sprint(version)

	log.Println("Автообновление: " + u)

	// request the new file
	resp, err := http.Get(u)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	updbuf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	log.Println("Автообновление: Обновление длинной.", len(updbuf))
	if len(updbuf) < 1000 {
		log.Println("Автообновление: Обновление не требуется.")
		return false
	}

	err = ioutil.WriteFile(appName+".new", updbuf, 0777)
	log.Println("Автообновление: Применение обновления.", err)
	err = os.Remove(appName + ".old")
	log.Println("Автообновление: step0", err)
	time.Sleep(1 * time.Second)
	err = os.Rename(appName, appName+".old")
	log.Println("Автообновление: step1", err)
	time.Sleep(1 * time.Second)
	err = os.Rename(appName+".new", appName)
	log.Println("Автообновление: step2", err)
	time.Sleep(1 * time.Second)

	return true
}

func doRestart() {
	log.Println("Перезапуск приложения.")
	if isServer {
		cmd := exec.Command(appName, "wait", "server")
		cmd.Start()
	} else {
		cmd := exec.Command(appName, "wait")
		cmd.Start()
	}

	os.Exit(0)
}

func savePacket(packet progInfo) {
	f, _ := os.OpenFile("packet.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	buf, _ := json.MarshalIndent(packet, "", " ")
	f.WriteString(string(buf))
	f.Close()
}

func main() {
	// defer profile.Start().Stop()
	// проверяем что это не перезапуск
	for _, v := range os.Args {
		if v == "wait" {
			log.Println("Автообновление: ждем 5 секунд перед стартом ...")
			time.Sleep(5 * time.Second)
		}
		if v == "debug" {
			debug = true
			continue
		}
		if v == "server" {
			isServer = true
			continue
		}
	}
	_, err := os.Stat("apt.srv")
	if err == nil {
		// файл признака сервера
		isServer = true
	}

	guiLastWork = make(chan progInfo)
	selfupdate = make(chan string)
	//настройка логирования
	f_log, err := os.OpenFile("apt.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Println("error opening log file: %v", err)
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	defer f_log.Close()

	wrt := io.MultiWriter(f_log, os.Stdout)
	log.SetOutput(wrt)
	log.Println("APPNAME", appName, "VERSION", version)
	log.Println("LOGING setup ok")

	if isServer == false {
		// загурзка конфигурации

		cfgMain.Settings.DB_CONFIG = "SYSDBA:masterkey@localhost:3052/d:\\iadb\\IAPTEKA.fdb"
		cfgMain.Settings.SRV_CONFIG = "localhost:8081"

		cfgMain.Settings.SMTP_SERVER = "smtp.yandex.ru:587"
		cfgMain.Settings.SMTP_FROM = "apteka error"
		cfgMain.Settings.SMTP_TO = "admiralu82@yandex.ru"
		cfgMain.Settings.SMTP_PASS = "oeexoztsqucsgnqh"
		cfgMain.Settings.SMTP_SUBJECT = "Subject"

		cfg1 := cfgTask{ShedType: "ExServer", Start: 7, Stop: 21, Day: 127, Interval: 20,
			BinExe: "ExServerExe.exe", Path: "c:\\IAUtils\\ExServer"}

		cfg2 := cfgTask{ShedType: shedSelfRestart, Start: 3, Stop: 3, Day: 1, Interval: 60}

		cfg3 := cfgTask{ShedType: "UServer", Start: 2, Stop: 2, Day: 127, Interval: 60,
			BinExe: "UServer.exe", Path: "C:\\IADistrib\\Distrib\\Utils\\AutoUpdate"}

		cfg := make([]cfgTask, 0)
		cfgMain.Tasks = append(cfg, cfg1, cfg2, cfg3)

		_, err = os.Stat("apt.cfg")
		if err == nil {
			// файл конфигурации существует
			bug_cfg, _ := ioutil.ReadFile("apt.cfg")
			err := yaml.Unmarshal(bug_cfg, &cfgMain)
			if err != nil {
				log.Panic("Ошибка в файле кнофигурации.", err)
			}
			// запишем конфигурацию для теста
			f_cfg, _ := os.OpenFile("apt.test.cfg", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
			buf_cfg, _ := yaml.Marshal(&cfgMain)
			f_cfg.WriteString(string(buf_cfg))
			f_cfg.Close()

		} else {
			// конфигурации нет - запишем её для образца
			f_cfg, _ := os.OpenFile("apt.cfg", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
			// buf_cfg, _ := json.MarshalIndent(cfgMain, "", " ")
			buf_cfg, _ := yaml.Marshal(&cfgMain)
			f_cfg.WriteString(string(buf_cfg))
			f_cfg.Close()
		}
		log.Println("Текущая конфигурация:")
		log.Println(cfgMain)
	}

	// поток самообновления

	if isServer == false {
		log.Println("Запуск службы самообновления.")
		go func() {
			for {
				select {
				case st := <-selfupdate:
					if st == "restart" {
						log.Println("Начало перезапуска. Ожидание 60 сек.")
						time.Sleep(60 * time.Second)
						doRestart()
					}
					if st == "update" {
						if doUpdate() {
							log.Println("Начало перезапуска")
							time.Sleep(2 * time.Second)
							doRestart()
						}
					}
				default:
					time.Sleep(15 * time.Second)
					_, min, _ := time.Now().Clock()
					if min%10 == 0 {
						if doUpdate() {
							log.Println("Начало перезапуска")
							time.Sleep(2 * time.Second)
							doRestart()
						}
					}
				}
			}
		}()
	}

	if isServer == false {
		selfupdate <- "update"
	}

	if isServer {
		go Webserver()
	} else {
		// запускаем планировщик
		go sheduler()
	}

	// настраиваем главное окно
	guiInit(guiLastWork)
}

func guiInit(guiChan chan progInfo) {
	log.Println("GUI init begin")

	var guiLastInfo []progInfo
	guiLastInfo = make([]progInfo, 0)

	go func() {
		for {
			select {
			case t := <-guiChan:
				guiLastInfo = append(guiLastInfo, t)
			default:
				time.Sleep(1 * time.Second)
			}
		}
	}()

	mw, _ := walk.NewMainWindow()
	ni, _ := walk.NewNotifyIcon()
	defer ni.Dispose()

	if isServer {
		ni.SetIcon(walk.IconShield())
		ni.SetToolTip("Инфоаптека сервер")
	} else {
		ni.SetIcon(walk.IconInformation())
		ni.SetToolTip("Инфоаптека клиент")
	}

	if isServer == false {
		// по нажатию левой кнопки мыши выдаем статистику работы
		ni.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
			if button != walk.LeftButton {
				return
			}

			lll := len(guiLastInfo)

			msg := "Задач ещё не было."
			if lll > 0 {
				start := 0
				if lll > 2 {
					start = lll - 3
				}
				disp := guiLastInfo[start:lll]
				msg = ""
				for _, v := range disp {

					msg += fmt.Sprintln(v.TASKSTATUS.SHED_TYPE, v.TASKSTATUS.STOP.Format("2006-01-02 15:04"), "(", v.TASKSTATUS.EXITCODE, ")")
				}
			}

			ni.ShowCustom("Последние задачи.", msg)
		})
	} else {
		// по нажатию левой кнопки мыши выдаем статистику работы
		ni.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
			if button != walk.LeftButton {
				return
			}

			ni.ShowCustom("Клиентов подключено.", "")
		})

	}

	if isServer == false {

		// меню конфигурация
		cfgAction := walk.NewAction()
		cfgAction.SetText("Конфигурация")
		cfgAction.Triggered().Attach(func() {

			c := exec.Command("notepad.exe", "apt.cfg")
			c.Start()

		})
		ni.ContextMenu().Actions().Add(cfgAction)

		// меню обмена
		obmenAction := walk.NewAction()
		obmenAction.SetText("Обмен")
		obmenAction.Triggered().Attach(func() {
			for _, v := range cfgMain.Tasks {
				if v.ShedType == "ExServer" {
					taskChan <- v
					// task(v)
					break
				}
			}
		})
		ni.ContextMenu().Actions().Add(obmenAction)
	}

	// меню остановить обмент
	stopAction := walk.NewAction()
	stopAction.SetText("Restart")
	stopAction.Triggered().Attach(func() { doRestart() })
	ni.ContextMenu().Actions().Add(stopAction)

	// меню выход
	if isServer {

		exitAction := walk.NewAction()
		exitAction.SetText("Выход")
		exitAction.Triggered().Attach(func() { walk.App().Exit(0) })
		ni.ContextMenu().Actions().Add(exitAction)
	}

	// The notify icon is hidden initially, so we have to make it visible.
	ni.SetVisible(true)
	//ni.ShowInfo("Walk NotifyIcon Example", "Click the icon to show again.")

	// Run the message loop.
	log.Println("GUI запущен")
	mw.Run()
}

func sheduler() {
	log.Println("Планировщик: запущен")

	// запускаем клиента веб
	sendChan := make(chan progInfo, 100)
	cmdrcvChan := make(chan progInfo, 100)
	taskChan = make(chan cfgTask)

	go WebcientAptstat(sendChan, cmdrcvChan)

	go func() {
		taskToWork := make([]cfgTask, 0)

		for {
			time.Sleep(1 * time.Second)
			select {
			case t := <-taskChan:
				taskToWork = append(taskToWork, t)
				log.Println("Планировщик: задача добавлена.", t)
			default:
				if len(taskToWork) > 0 {

					lens := len(taskToWork)
					ttt := taskToWork[lens-1]

					if ttt.ShedType == shedSelfRestart && len(taskToWork) > 1 {
						// задание рестарта не последнее, переносим её назад
						taskToWork = append([]cfgTask{ttt}, taskToWork[:lens-1]...)
						ttt = taskToWork[lens-1]
					}

					stat := task(ttt)
					packetTask := progInfo{TASKSTATUS: stat}

					//отправляем пакет stat на отправку серверу
					sendChan <- packetTask
					guiLastWork <- packetTask
					time.Sleep(1 * time.Second)

					taskToWork = taskToWork[:lens-1]
				}
			}
		}

	}()

	// главный цикл планировщика
	min_last := 1110
	for {

		// узнаем время и день
		hr, min, _ := time.Now().Clock()
		day := int(time.Now().Weekday())
		if day == 0 {
			day = day + 7
		}
		day = 1 << uint(day-1)

		// проверка что в это время уже выполнялось все
		if min == min_last {
			time.Sleep(5 * time.Second)
			continue
		}
		min_last = min

		for _, conf := range cfgMain.Tasks {

			// проверка дня
			if conf.Day&day == 0 {
				// задача не выполняется в этот день
				continue
			}
			// проверка часов
			if conf.Start <= hr && conf.Stop >= hr && min%conf.Interval == 0 && conf.Interval >= 10 {
				//запускаем задачу
				taskChan <- conf
				continue
			}
		}

		// обработка входящих комманд от сервера
		select {
		case cmd := <-cmdrcvChan:
			//ищем conf по cmd
			for _, v := range cfgMain.Tasks {
				if cmd.TASKSTATUS.SHED_TYPE == v.ShedType {
					taskChan <- v
					break
				}
			}

			// добавлено если Restart и Hello отсутствуют в конфигурации планировщике
			if cmd.TASKSTATUS.SHED_TYPE == shedSelfRestart || cmd.TASKSTATUS.SHED_TYPE == shedHello {
				work := cfgTask{ShedType: cmd.TASKSTATUS.SHED_TYPE}
				taskChan <- work
				break
			}

		default:
			break
		}
		// задания не должны пересекаться
		// завершение или проверка процессов перед запуском
	}

}

func task(cfg cfgTask) taskStatus {

	status := taskStatus{SHED_TYPE: cfg.ShedType}
	status.START = time.Now()

	if cfg.ShedType == shedHello {
		status.START = time.Now()
		status.STOP = time.Now()
		status.LOG = ""
		return status
	}

	if cfg.ShedType == shedSelfRestart {
		status.START = time.Now()
		status.STOP = time.Now()
		status.LOG = ""

		selfupdate <- "restart"
		return status
	}

	// убиваем старые процессы
	killProc(cfg.BinExe)

	// сформировать комманду с полным путем !!!!!!!!!!!!!!!!!!!!!!!!!
	exePath := filepath.Join(cfg.Path, cfg.BinExe)

	//меняем текущую папку
	old_cwd, err := os.Getwd()
	if cfg.WorkDir == "" {
		err = os.Chdir(filepath.Clean(cfg.Path))
	} else {
		err = os.Chdir(filepath.Clean(cfg.WorkDir))
	}

	var cmd *exec.Cmd
	if len(cfg.Param) == 0 {
		cmd = exec.Command(exePath)
	} else {
		cmd = exec.Command(exePath, cfg.Param...)
	}

	log.Println("Планировщик: задание ", cfg.ShedType, " запущено. (", exePath, cfg.Param, ")")
	var buf bytes.Buffer
	cmd.Stdout = &buf

	err = cmd.Start()

	if err != nil {
		log.Println("Планировщик: ошибка запуска задания ", cfg.ShedType, err)
	}
	// ожидание завершения работы
	done := make(chan string)

	// go waitForStop(done, cmd)
	go func(done chan string, cmd *exec.Cmd) {
		ret := cmd.Wait()
		if ret == nil {
			done <- ""
		} else {
			done <- ret.Error()
		}

	}(done, cmd)

	for {
		time.Sleep(1 * time.Second)
		select {
		case ret := <-done:
			status.EXITCODE += " " + ret
			// log.Println("Завешен обмен")
			break
		default:

			// если ожидание превысит 20 минут то убиваем процесс
			if time.Now().Unix()-status.START.Unix() > int64(20*60) {
				status.EXITCODE = "100. Принудительное."

				cmd.Process.Kill()
				time.Sleep(5 * time.Second)

				killProc(cfg.BinExe)
				// log.Println(cfg.ShedType, " !!!! long time  obmen")

				break
			}
			continue
		}
		break
	}

	// завершение задачи
	//вернем пути назад
	err = os.Chdir(old_cwd)

	status.LOG, _ = charmap.CodePage866.NewDecoder().String(string(buf.String()))

	log.Println("Планировщик: задание:", cfg.ShedType, " закончено. ExitCode=", status.EXITCODE)
	status.STOP = time.Now()

	if status.LOG == "" {
		status.LOG = fmt.Sprintln(exePath, cfg.Param)
	}

	return status
}

func killProc(bin string) {
	if bin == "" {
		return
	}
	cmdKill := exec.Command("TASKKILL.EXE", "/F", "/IM", bin, "/T")
	err := cmdKill.Run()
	if err != nil && debug {
		log.Printf("TASKKILL "+bin+" error: %v", err)
	}
}

func Webserver() {
	log.Println("WS Server start...")

	db, err := sql.Open("sqlite3", "apt.db")
	if err != nil {
		log.Panic("WS Server SQL open error:", err)
	}
	dbSrv = db
	defer db.Close()

	// проверка таблиц

	db.Exec("CREATE TABLE if not exists  CLIENTS (APT_VERSION	INTEGER, SYSVER TEXT, PRED_ID TEXT, NODE_ID	TEXT UNIQUE,  NODE_NAME	TEXT,NODE_CFG TEXT, PRIMARY KEY(NODE_ID))")
	db.Exec("CREATE TABLE if not exists  LOGS (NODE_ID	TEXT, TYPE TEXT, DATE_START TEXT, DATE_END TEXT,EXITCODE TEXT,LOG TEXT);")
	db.Exec("CREATE TABLE if not exists  EXE_VERSION (	NODE_ID TEXT, NAME TEXT, VERSION TEXT,VERSION_OLD TEXT);")

	http.HandleFunc("/aptstat", SrvAptstat)
	http.HandleFunc("/aptupdate", SrvAptUpdate)
	http.HandleFunc("/clients", SrvClients)

	log.Println(http.ListenAndServe("0.0.0.0:8081", nil))
}

func SrvAptUpdate(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["ver"]
	log.Println("Сервер автообновления: клиент подключился", r.RemoteAddr, r.RequestURI)

	if !ok || len(keys[0]) < 1 {
		log.Println("Сервер автообновления: ключ 'ver' отсутствует.")
		return
	}

	value, err := strconv.ParseInt(keys[0], 10, 32)
	if err != nil {
		log.Println("Сервер автообновления: ключ 'ver' ошибка формата", err)
		return
	}

	if value >= version {
		return
	}

	file, err := os.Open(appName)
	if err != nil {
		log.Println(err)
		return
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		log.Println(err)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+appName)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(int(fi.Size())))

	log.Println("Сервер автообновления: Отправка обновления.", r.RemoteAddr)
	io.Copy(w, file)

	log.Println("Сервер автообновления: Обновление получено клиентом.", r.RemoteAddr)
}

type clientsInfo struct {
	Con  *websocket.Conn
	Addr string
	Info progInfo
}

var clients map[string]clientsInfo = make(map[string]clientsInfo)

func SrvClients(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["raw"]
	log.Println("WS SrvClients: клиент подключился", r.RemoteAddr, r.RequestURI)

	if ok && len(keys) > 0 {
		log.Println("WS SrvClients: клиент подключился", r.RemoteAddr, r.RequestURI)
		buf, _ := json.MarshalIndent(&clients, "", "  ")
		w.Write(buf)
		return
	}

	// сохранить пакет в базе данных CLIENTS
	rows, err := dbSrv.Query("SELECT APT_VERSION, SYSVER, PRED_ID, NODE_ID, NODE_NAME,	 NODE_CFG FROM CLIENTS")

	if err != nil {
		// w.Write(err.Error())
		log.Println("WS SrvClients: ошибка SQL", err)
		return
	}

	var info map[string]clientsInfo = make(map[string]clientsInfo)
	for rows.Next() {

		var apt_version int
		var node_id, sysver, pred_id, node_name, node_cfg string
		err = rows.Scan(&apt_version, &sysver, &pred_id, &node_id, &node_name, &node_cfg)

		info[node_id] = clientsInfo{Info: progInfo{
			APT_VERSION: apt_version,
			NODE_ID:     node_id,
			NODE_NAME:   node_name,
			SYSVER:      sysver,
			NODE_CFG:    node_cfg,
			TASKSTATUS:  taskStatus{},
		}}
	}

	for k, v := range clients {
		_, ok := info[k]
		if ok == false {
			log.Println("Ошибочный ключ в clients", k)
			continue
		}
		tmp := info[k]
		tmp.Info.TASKSTATUS = v.Info.TASKSTATUS
		info[k] = tmp
	}

	fmt.Fprint(w, `<html><head><title>Перечень аптек</title><script language="JavaScript">document.addEventListener('DOMContentLoaded', () => {

		const getSort = ({ target }) => {
			const order = (target.dataset.order = -(target.dataset.order || -1));
			const index = [...target.parentNode.cells].indexOf(target);
			const collator = new Intl.Collator(['en', 'ru'], { numeric: true });
			const comparator = (index, order) => (a, b) => order * collator.compare(
				a.children[index].innerHTML,
				b.children[index].innerHTML
			);
			
			for(const tBody of target.closest('table').tBodies)
				tBody.append(...[...tBody.rows].sort(comparator(index, order)));
	
			for(const cell of target.parentNode.cells)
				cell.classList.toggle('sorted', cell === target);
		};
		
		document.querySelectorAll('.table_sort thead').forEach(tableTH => tableTH.addEventListener('click', () => getSort(event)));
		
	});</script></head><body>`)
	fmt.Fprint(w, `	<style type="text/css">
	.table_sort table {
		border-collapse: collapse;
	}
	
	.table_sort th {
		color: #ffebcd;
		background: #008b8b;
		cursor: pointer;
	}
	
	.table_sort td,
	.table_sort th {
		width: 150px;
		height: 40px;
		text-align: center;
		border: 2px solid #846868;
	}
	
	.table_sort tbody tr:nth-child(even) {
		background: #e3e3e3;
	}
	
	th.sorted[data-order="1"],
	th.sorted[data-order="-1"] {
		position: relative;
	}
	
	th.sorted[data-order="1"]::after,
	th.sorted[data-order="-1"]::after {
		right: 8px;
		position: absolute;
	}
	
	th.sorted[data-order="-1"]::after {
		content: "▼"
	}
	
	th.sorted[data-order="1"]::after {
		content: "▲"
	}
	</style>`)
	fmt.Fprint(w, `<table class="table_sort"><caption>Перечень аптек</caption>`)

	fmt.Fprint(w, "<thead><th>ID</th><th>Имя аптеки</th><th>Клиент</th><th>Версия БД</th><th>Пследнее задание</th>")
	fmt.Fprint(w, "<th>Минут прошло</th><th>Код возврата</th><th>Обмен</th></thead><tbody>")

	var i int
	for _, v := range info {
		fmt.Fprint(w, "<tr>")
		fmt.Fprintf(w, "<td>%02s</td>", v.Info.NODE_ID)
		fmt.Fprint(w, "<td>", v.Info.NODE_NAME, "</td>")
		fmt.Fprint(w, "<td>", v.Info.APT_VERSION, "</td>")
		fmt.Fprint(w, "<td>", v.Info.SYSVER, "</td>")

		if v.Info.TASKSTATUS.SHED_TYPE == "" {
			fmt.Fprint(w, "<td></td><td></td><td></td><td></td>")
		} else {
			i++
			fmt.Fprint(w, "<td>", v.Info.TASKSTATUS.SHED_TYPE, "</td>")
			fmt.Fprint(w, "<td>", int((time.Now().Sub(v.Info.TASKSTATUS.STOP)).Minutes()), "</td>")
			fmt.Fprint(w, "<td>", v.Info.TASKSTATUS.EXITCODE, "</td>")
			fmt.Fprint(w, "<td><a target=\"_blank\" href=\"http://apt.lab31.ru:55000/aptstat?node_id="+v.Info.NODE_ID+"&shed_type=ExServer\">Обмен</a>")
		}
		fmt.Fprint(w, "</tr>")
	}

	fmt.Fprint(w, `</tbody></table>`)
	fmt.Fprint(w, `</body></html>`)
	fmt.Fprint(w, "<h1> Всего 		", len(info), " аптек</h1>")
	fmt.Fprint(w, "<h1> Подключено 	", i, " аптек</h1>")

	// tmpl, _ := template.New("data").Parse(temlMain)
	// tmpl.Execute(w, info)

}

func SrvAptstat(w http.ResponseWriter, r *http.Request) {
	var upgrader = websocket.Upgrader{} // use default options
	log.Println("WS SrvAptstat: клиент подключился", r.RemoteAddr, r.RequestURI)

	// проверяем ключи если есть node_id и shed_type заначит это комманда клиенту
	keys_node, _ := r.URL.Query()["node_id"]
	keys_type, _ := r.URL.Query()["shed_type"]

	if len(keys_node) == 1 && len(keys_type) == 1 {
		// обработка входящего запроса и отпавка комманды клиенту
		conInfo, ok_con := clients[keys_node[0]]
		if ok_con == false {
			log.Println("WS SrvAptstat: Клиента с node_id=", keys_node[0], " нет.")
			return
		}

		packet := progInfo{
			NODE_ID:    keys_node[0],
			TASKSTATUS: taskStatus{SHED_TYPE: keys_type[0]},
		}

		conInfo.Con.WriteJSON(&packet)
		return
	}

	if len(r.URL.Query()) > 0 {
		// обработка ошибочного запроса
		log.Println("WS SrvAptstat:  'node_id= shed_type=' ошибка в параметрах", r.URL.Query())
		return
	}

	// запрос на соединение
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print("WS SrvAptstat: ошибка создания websocket:", err)
		return
	}
	defer c.Close()

	// надо сохранить с и node_id в clients
	first_packet := true
	var first_node_id string

	for {
		var packet progInfo
		err := c.ReadJSON(&packet)
		if err != nil {
			log.Println("WS SrvAptstat: JSON read error:", err)
			break
		}
		if debug {
			log.Println("WS SrvAptstat:  recv:", packet)
		}
		if first_packet {
			clients[packet.NODE_ID] = clientsInfo{
				Addr: r.RemoteAddr,
				Info: packet,
				Con:  c,
			}

			first_node_id = packet.NODE_ID
			first_packet = false
		}

		// сохранить пакет в базе данных CLIENTS
		row := dbSrv.QueryRow("SELECT APT_VERSION, SYSVER, PRED_ID, NODE_ID, NODE_NAME,	 NODE_CFG FROM CLIENTS where NODE_ID = $1", packet.NODE_ID)

		var apt_version int
		var node_id, sysver, pred_id, node_name, node_cfg string
		err = row.Scan(&apt_version, &sysver, &pred_id, &node_id, &node_name, &node_cfg)

		if err == sql.ErrNoRows {
			log.Println("WS SrvAptstat:  SQL - добавляем запись")
			_, err := dbSrv.Exec("insert into CLIENTS (APT_VERSION, SYSVER, PRED_ID, NODE_ID, NODE_NAME, NODE_CFG) values ($1, $2, $3, $4 ,$5,$6)",
				packet.APT_VERSION, packet.SYSVER, packet.PRED_ID, packet.NODE_ID, packet.NODE_NAME, packet.NODE_CFG)
			if err != nil {
				log.Println("WS SrvAptstat:  SQL error", err)
			}
		} else {
			//обновляем запись
			if packet.NODE_NAME != node_name || packet.PRED_ID != pred_id ||
				packet.APT_VERSION != apt_version || packet.NODE_CFG != node_cfg {
				dbSrv.Exec("update CLIENTS set APT_VERSION = $1, SYSVER=$2, PRED_ID=$3, NODE_NAME=$4, NODE_CFG=$5 where NODE_ID = $6",
					packet.APT_VERSION, packet.SYSVER, packet.PRED_ID, packet.NODE_NAME, packet.NODE_CFG, packet.NODE_ID)
				log.Println("WS SrvAptstat: SQL - запись обновлена")

			}
		}

		// сохранить пакет в базе данных EXE_VERSION
		var name, version, version_old string
		var update bool = false
		var version_db = make(map[string]string)
		rows, err := dbSrv.Query("SELECT * FROM EXE_VERSION where NODE_ID = $1", packet.NODE_ID)
		if err != nil {
			log.Println("WS SrvAptstat:  SQL error", err)
		} else if err == sql.ErrNoRows {
			// добавляем все записи в первый раз
			update = true

		} else {
			var version_db = make(map[string]string)

			for rows.Next() {
				err = rows.Scan(&node_id, &name, &version, &version_old)
				if err != nil {
					log.Println("WS SrvAptstat: SQL error", err)
				} else {
					version_db[name] = version
				}
			}
			if equal(version_db, packet.EXE_VERSION) == false {
				update = true

			}
		}

		if update {
			_, err := dbSrv.Exec("delete from EXE_VERSION where NODE_ID = $1", packet.NODE_ID)
			if err != nil {
				log.Println("WS SrvAptstat:  Delete error", err)
			} else {
				for k, v := range packet.EXE_VERSION {
					_, err := dbSrv.Exec("insert into EXE_VERSION (NODE_ID, NAME, VERSION, VERSION_OLD)  values ($1, $2, $3, $4)",
						packet.NODE_ID, k, v, version_db[k])

					if debug {
						log.Println(err, packet.NODE_ID, k, v, version_db[k])
					}

				}
			}
		}

		// добавление в базу пакета статистики
		_, err = dbSrv.Exec("insert into LOGS (NODE_ID, TYPE, DATE_START, DATE_END, EXITCODE, LOG) values ($1, $2, $3, $4, $5, $6)",
			packet.NODE_ID, packet.TASKSTATUS.SHED_TYPE, packet.TASKSTATUS.START.Format("2006-01-02 15:04:05"),
			packet.TASKSTATUS.STOP.Format("2006-01-02 15:04:05"), packet.TASKSTATUS.EXITCODE,
			packet.TASKSTATUS.LOG)

		if debug {
			log.Println("WS SrvAptstat: error: ", err)
		}

		//обновим информацию о последнем пакете
		vv := clients[first_node_id]
		vv.Info.TASKSTATUS = packet.TASKSTATUS
		clients[first_node_id] = vv

	}

	// удаляем информацию из clients
	_, ok := clients[first_node_id]
	if ok {
		delete(clients, first_node_id)
	}
}

func equal(map1, map2 map[string]string) bool {
	if len(map1) != len(map2) {
		return false
	}

	for v, k := range map1 {
		if map2[k] != v {
			return false
		}
	}

	return true
}

/////////////////////////////////////////////////////////

func WebcientAptstat(sendChan chan progInfo, cmdrcvChan chan<- progInfo) {

	var addr_srv string = cfgMain.Settings.SRV_CONFIG
	var packetsToSend []progInfo = make([]progInfo, 0)
	var packetsToRead []progInfo = make([]progInfo, 0)

	if addr_srv == "" {
		log.Panic("WS Client Aptstat: адрес сервера не задан. Поправте конфигурацию")
	}

	u := url.URL{Scheme: "ws", Host: addr_srv, Path: "/aptstat"}

	for { //for установки соединения

		// устанавливаем соединение
		log.Println("WS Client Aptstat: подключаемся к ", u.String())
		c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			log.Println("WS Client Aptstat: ошибка установки соединния: ", err)
			time.Sleep(10 * time.Second)
			continue
		}
		defer c.Close()
		log.Println("WS Client Aptstat: соединение установленно.")

		// sql запрос к базе для подготовки hello пакета
		packetInfo := sqlINFO()
		packetInfo.TASKSTATUS = taskStatus{
			SHED_TYPE: shedHello,
			START:     time.Now(),
			STOP:      time.Now(),
		}

		readChan := make(chan progInfo)

		packetsToSend = append(packetsToSend, packetInfo)

		////////////////////////////////////////////////////////
		// поток приема сообщений
		go func() {
			packetToRead := progInfo{}
			for {
				if c == nil {
					break
				}
				err := c.ReadJSON(&packetToRead)
				if err == nil {
					//если ошибки нет то отправляем пакет в поток прочитанных
					readChan <- packetToRead
				} else {
					//если ошибка то ошибка
					log.Println("WS Client read error", err)

					// посылаем пустой пакет для завершения соединения
					readChan <- progInfo{APT_VERSION: -1}
					break
				}
			}
		}()
		/////////////////////////////////////////////////////////

		for { //for прием в буфер пакетов
			time.Sleep(100 * time.Millisecond)
			select {
			case t := <-sendChan:
				// берем стартовый пакет и заменяем у него TASKSTATUS
				packetInfo.TASKSTATUS = t.TASKSTATUS
				packetsToSend = append(packetsToSend, packetInfo)
				continue
			case t := <-readChan:
				if t.APT_VERSION == -1 {
					//завершаем соединение
					break
				}
				packetsToRead = append(packetsToRead, t)
				continue
			default:
				// обработка исходящих пакетов
				lens := len(packetsToSend)
				if lens > 0 {
					buf, _ := json.Marshal(packetsToSend[lens-1])
					err := c.WriteMessage(websocket.TextMessage, buf)

					if err != nil {
						log.Println("WS Client Aptstat: ошибка записи пакета:", err)
						break
					} else {
						// пакет успешно отправлен
						// удаляем пакет
						packetsToSend = packetsToSend[:lens-1]
					}
				}

				// обработка входящих пакетов
				lens = len(packetsToRead)
				if lens > 0 {
					packet := packetsToRead[lens-1]
					log.Println("WS Client Aptstat: Входящий пакет для обработки.", packet.TASKSTATUS.SHED_TYPE)

					// отправка пакета на выполение в планировщик
					cmdrcvChan <- packet

					// удаление пакета
					packetsToRead = packetsToRead[:lens-1]
				}
				continue
			}
			break
		} //for прием в буфер пакетов
		log.Println("WS Client Aptstat: завершение соединения.")
	} //for установки соединения
}

////////////////////////////////////////////////////////////

func sendEmail(body string) {
	from := cfgMain.Settings.SMTP_FROM
	pass := cfgMain.Settings.SMTP_PASS
	to := cfgMain.Settings.SMTP_TO

	msg := "From: " + from + "\n" +
		"To: " + to + "\n" +
		"Subject: " + cfgMain.Settings.SMTP_SUBJECT + "\n\n" +
		body

	err := smtp.SendMail(cfgMain.Settings.SMTP_SERVER,
		smtp.PlainAuth("", from, pass, strings.Split(cfgMain.Settings.SMTP_SERVER, ":")[0]),
		from, []string{to}, []byte(msg))

	if err != nil {
		log.Println("Отправка почты: ошибка.", err)
		return
	}

	log.Println("Отправка почты: ")
	log.Println(msg)
}

// var temlMain string = `
// <html>
// <head>
//     <title>Something here</title>
// </head>
// <body>
// <table>
// {{if .}}
// {{ range $key, $value := . }}
// <tr>
//    <td>{{ $key }}</td>
//    <td>{{ $value.Info.NODE_NAME }}</td>
//    <td>{{ $value.Info.APT_VERSION }}</td>
//    <td>{{ $value.Info.SYSVER }}</td>
//    <td>{{if $value.Info.SHED_TYPE}}{{ $value.Info.TASKSTATUS.STOP }} {{end}}</td>
//    <td>{{if $value.Info.SHED_TYPE}} $value.Info.SHED_TYPE {{end}}</td>
//    <td><a href="http://localhost">Обмен</a>
// </tr>
// {{ end }}
// {{else}}
// <tr>
// <td>nothing</td>
// </tr>
// {{end}}
// </table>
// </body>
// `
