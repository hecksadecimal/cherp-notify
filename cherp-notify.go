package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/systray"
	"github.com/google/go-cmp/cmp"
	"github.com/hecksadecimal/cherp-notify/icon"
	cookiejar "github.com/juju/persistent-cookiejar"
	"github.com/martinlindhe/notify"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/publicsuffix"
)

var lock sync.Mutex
var gClient http.Client
var gNotifObject map[string]interface{}
var gStop chan bool

// Typically used on first start to get a cookie for all future sessions.
// Username and password are *never* saved.
func login(c *http.Client, user string, pass string) (success bool) {
	resp, err := c.PostForm("https://cherp.chat/api/user/login", url.Values{"username": {user}, "password": {pass}})
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	fmt.Println("Response status:", resp.Status)

	var jsonData map[string]interface{}
	scanner := bufio.NewScanner(resp.Body)
	for i := 0; scanner.Scan() && i < 5; i++ {
		data := scanner.Text()
		log.Println(data)
		json.Unmarshal([]byte(data), &jsonData)
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
	if jsonData["status"].(string) != "success" {
		return false
	}
	return true
}

// Load loads the file at path into v.
// Use os.IsNotExist() to see if the returned error is due
// to the file being missing.
func Load(path string, v interface{}) error {
	lock.Lock()
	defer lock.Unlock()
	f, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(f, v)
}

// Save saves a representation of v to the file at path.
func Save(path string, v interface{}) error {
	lock.Lock()
	defer lock.Unlock()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r, err := json.Marshal(v)
	if err != nil {
		return err
	}
	rread := bytes.NewReader(r)
	_, err = io.Copy(f, rread)
	return err
}

func copyMap(m map[string]interface{}) map[string]interface{} {
	cp := make(map[string]interface{})
	for k, v := range m {
		vm, ok := v.(map[string]interface{})
		if ok {
			cp[k] = copyMap(vm)
		} else {
			cp[k] = v
		}
	}

	return cp
}

func sendNotification() {
	notify.Notify("Cherp Notify", "Cherp Notifier", "You have new prompt replies", "")
}

// The main logic behind detecting new replies.
func getNotifs(c *http.Client, n *map[string]interface{}) {
	o := make(map[string]interface{})
	o = copyMap(*n)
	resp, err := c.Get("https://cherp.chat/api/chat/list/unread")
	if err != nil {
		log.Println(err)
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	for i := 0; scanner.Scan() && i < 5; i++ {
		json.Unmarshal([]byte(scanner.Text()), n)
		// This'll let us restore our state if the program is closed and run again.
		Save("./notifstate.json", n)
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
	// On our first run, our notifstate would not have been saved already.
	// We'll initialize the value here and also check to see if there's something unread.
	if len(o) == 0 {
		o = copyMap(*n)
		if len((*n)["chats"].([]interface{})) > 0 {
			log.Println("New notification detected.")
			sendNotification()
		}
	}
	// Always have the systray icon red if there's at least one unread reply.
	if len((*n)["chats"].([]interface{})) > 0 {
		systray.SetTemplateIcon(icon.Notif, icon.Notif)
	} else {
		systray.SetTemplateIcon(icon.Default, icon.Default)
	}
	if !cmp.Equal(o, *n) {
		// A simple comparison, if there's more unread chats than the last time we checked, trigger a notification.
		// TODO: Compare the two lists more thoroughly. It's possible to check a chat and get a new reply in another within the interval.
		if len((*n)["chats"].([]interface{})) > len(o["chats"].([]interface{})) {
			log.Println("New notification detected.")
			sendNotification()
		} else if len((*n)["chats"].([]interface{})) < len(o["chats"].([]interface{})) {
			log.Println("You've checked on an unread reply.")
		}
	}
}

// This keeps our reply check logic running on an interval.
func schedule(c *http.Client, n *map[string]interface{}, what func(*http.Client, *map[string]interface{}), delay time.Duration) chan bool {
	stop := make(chan bool)

	go func(c *http.Client, n *map[string]interface{}) {
		for {
			what(c, n)
			select {
			case <-time.After(delay):
			case <-stop:
				return
			}
		}
	}(c, n)

	return stop
}

func startPolling(c *http.Client, n *map[string]interface{}) {
	go getNotifs(c, n)
}

func onReady() {
	systray.SetTemplateIcon(icon.Default, icon.Default)
	systray.SetTitle("Cherp Notify")
	systray.SetTooltip("Cherp Notify")
	mQuitOrig := systray.AddMenuItem("Quit", "Quit the whole app")
	go func() {
		<-mQuitOrig.ClickedCh
		systray.Quit()
	}()

	log.Println("Polling for unread chats...")
	gStop = schedule(&gClient, &gNotifObject, startPolling, 10*time.Second)
	time.Sleep(25 * time.Millisecond)
	runtime.Goexit()
}

func main() {
	// Loading of the last saved notification state.
	if _, err := os.Stat("./notifstate.json"); err == nil {
		Load("./notifstate.json", &gNotifObject)
	}
	// Loading of the cookie jar
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List, Filename: "cookies.txt"})
	if err != nil {
		log.Fatal(err)
	}

	gClient = http.Client{
		Jar: jar,
	}

	u, err := url.Parse("https://cherp.chat")
	if err != nil {
		log.Fatal(err)
	}

	// If there's no cookies, we're going to need to log in for the first time to set them.
	if len(jar.Cookies(u)) == 0 {
		fmt.Println("First Time Setup")
		fmt.Println("---------------------")
		fmt.Print("USER: ")
		var user string
		fmt.Scanln(&user)
		fmt.Print("PASS: ")
		pass, err := terminal.ReadPassword(int(syscall.Stdin))
		if err != nil {
			panic(err)
		}
		fmt.Println()

		success := login(&gClient, user, string(pass))
		if !success {
			log.Fatalln("FATAL: Login failure, cannot continue.")
			os.Exit(1)
		}
		jar.Save()
	}

	onExit := func() {
		gStop <- true
		log.Println("Exiting...")
	}

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		systray.Quit()
	}()

	systray.Run(onReady, onExit)
}
