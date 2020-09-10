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
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-cmp/cmp"
	cookiejar "github.com/juju/persistent-cookiejar"
	"github.com/martinlindhe/notify"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/publicsuffix"
)

var lock sync.Mutex

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

func getNotifs(c *http.Client, n *map[string]interface{}) {
	o := make(map[string]interface{})
	o = copyMap(*n)
	resp, err := c.Get("https://cherp.chat/api/chat/list/unread")
	if err != nil {
		log.Fatal(err)
	}
	scanner := bufio.NewScanner(resp.Body)
	for i := 0; scanner.Scan() && i < 5; i++ {
		json.Unmarshal([]byte(scanner.Text()), n)
		Save("./notifstate.json", n)
	}
	if err := scanner.Err(); err != nil {
		panic(err)
	}
	// TODO: Change how changes are detected.
	if !cmp.Equal(o, *n) {
		if len((*n)["chats"].([]interface{})) > len(o["chats"].([]interface{})) {
			log.Println("New notification detected.")
			sendNotification()
		} else if len((*n)["chats"].([]interface{})) < len(o["chats"].([]interface{})) {
			log.Println("You've checked on an unread reply.")
		} else {
			//log.Println("No new replies yet...")
		}
	} else {
		//log.Println("No new replies yet...")
	}
}

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

func main() {
	var notifObject map[string]interface{}
	if _, err := os.Stat("./notifstate.json"); err == nil {
		Load("./notifstate.json", &notifObject)
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List, Filename: "cookies.txt"})
	if err != nil {
		log.Fatal(err)
	}

	client := &http.Client{
		Jar: jar,
	}

	u, err := url.Parse("https://cherp.chat")
	if err != nil {
		log.Fatal(err)
	}

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

		success := login(client, user, string(pass))
		if !success {
			log.Fatalln("FATAL: Login failure, cannot continue.")
			os.Exit(1)
		}
		jar.Save()
	}
	log.Println("Polling for unread chats...")
	stop := schedule(client, &notifObject, startPolling, 10*time.Second)
	time.Sleep(25 * time.Millisecond)
	runtime.Goexit()

	stop <- true
	fmt.Println("Saving state.")
}
