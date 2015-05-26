package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DisposaBoy/JsonConfigReader"
	ghttp "github.com/gorilla/http"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

type Tab struct {
	Description          string
	DevtoolsFrontendUrl  string
	FaviconUrl           string
	Id                   string
	Title                string
	Type                 string
	Url                  string
	WebSocketDebuggerUrl string
}

type ChromeError struct {
	Code    int      `json:"code,omitempty"`
	Message string   `json:"message,omitempty"`
	Data    []string `json:"data,omitempty"`
}

type RuntimeEvaluateRequest struct {
	Method string      `json:"method"`
	Params interface{} `json:"params"`
	Id     int         `json:"id"`
}

type RuntimeEvaluateRequestParams struct {
	Expression string `json:"expression"`
}

// We don't know if an incoming message from Chrome is going to be a request or
// a response. This will handle both.
type ChromeMessage struct {
	// Response Fields

	// Result map[string]interface{} `json:"result"`
	Result interface{} `json:"result,omitempty"`
	Error  interface{} `json:"error,omitempty"`

	// Request Fields

	// A String containing the name of the method to be invoked.
	Method string `json:"method"`
	// Object to pass as request parameter to the method.
	Params map[string]interface{} `json:"params"`
	// The request id. This can be of any type. It is used to match the
	// response with the request that it is replying to.
	// UPDATE: as of chrome 42.0.2311.90 it appears this must be a 32-bit int
	Id int `json:"id"`
}

type Configuration struct {
	PanesfeEndpoint     string `json:"panesfe_endpoint"`
	SlideTimeout        int64  `json:"slide_timeout"`
	PresentationTimeout int64  `json:"presentation_timeout"`
}

var config = Configuration{}

var logger *log.Logger

var host = "127.0.0.1"
var port = 2345

var next_url string

func main() {
	// Make random number randomish. Without this, calls to rand() will
	// always return the same sequence
	rand.Seed(time.Now().UnixNano())

	// Set up logging
	logger = log.New(os.Stdout, "", log.LstdFlags|log.Lshortfile)

	// default config
	config_filepath := os.Getenv("VIDEO_WALL_CONFIG_FILE")
	if config_filepath == "" {
		config_filepath = "/etc/video_wall_config.json"
	} else {
		logger.Println("config filepath is " + config_filepath)
	}
	config_file, err := os.Open(config_filepath)
	errCheck(err)

	// This makes the json a little more forgiving and allows comments
	stripped_config := JsonConfigReader.New(config_file)
	decoder := json.NewDecoder(stripped_config)
	err = decoder.Decode(&config)
	errCheck(err)
	config_file.Close()

	next_url = config.PanesfeEndpoint + "/presentations/next"

	var chrome *websocket.Conn

	presentationExpired := func(*Watchdog) {
		logger.Println("Watchdog expired. Loading next page.")
		// TODO: Flag the presentation as having a problem.
		pageDone(chrome)
	}

	slideWatchdog := Watchdog{
		"Slide Watchdog",
		time.Now().UnixNano(),
		config.SlideTimeout,
		false,
		presentationExpired,
	}

	presentationWatchdog := Watchdog{
		"Presentation Watchdog",
		time.Now().UnixNano(),
		config.PresentationTimeout,
		false,
		presentationExpired,
	}

	// loop read console messages
	go func() {
		chrome, err = getChrome()
		errCheck(err)
		pageDone(chrome)

		for {
			var chromeMessage ChromeMessage
			err := chrome.ReadJSON(&chromeMessage)
			if err != nil {
				logger.Println(err)
				logger.Println("Trying to reconnect to Chrome")
				chrome = nil
				chrome, err = getChrome()
				errCheck(err)
			} else {
				// logger.Println(chromeMessage)
				if len(chromeMessage.Method) != 0 {
					logger.Println("Message is a request")
					logger.Println(chromeMessage.Method)
					logger.Println(chromeMessage.Params)
					if chromeMessage.Method == "Page.domContentEventFired" {
						insertJavascript(chrome)
						slideWatchdog.Start()
						presentationWatchdog.Start()
					}
					// TODO: these type assertions are ugly and are somewhat likely to crash us here
					if chromeMessage.Method == "Console.messageAdded" &&
						chromeMessage.Params["message"].(map[string]interface{})["level"] == "info" &&
						chromeMessage.Params["message"].(map[string]interface{})["stackTrace"].([]interface{})[0].(map[string]interface{})["functionName"] == "GrowingPanes.done" {
						pageDone(chrome)
					}
					if chromeMessage.Method == "Console.messageAdded" &&
						chromeMessage.Params["message"].(map[string]interface{})["level"] == "info" &&
						chromeMessage.Params["message"].(map[string]interface{})["stackTrace"].([]interface{})[0].(map[string]interface{})["functionName"] == "GrowingPanes.keepAlive" {
						slideWatchdog.KeepAlive()
					}
				}
				if chromeMessage.Result != nil {
					logger.Println("Message is a response")
					logger.Println(chromeMessage.Result)
				}
				if chromeMessage.Error != nil {
					chromeError := chromeMessage.Error.(map[string]interface{})
					logger.Println("Message is an error")
					logger.Println(chromeError["code"])
					logger.Println(chromeError["message"])
					logger.Println(chromeError["data"])
				}
			}
		}
	}()

	r := mux.NewRouter()
	r.HandleFunc("/status", func(response http.ResponseWriter, request *http.Request) {
		if chrome == nil {
			fmt.Fprintln(response, "Status: Chrome disconnected")
		} else {
			fmt.Fprintln(response, "Status: OK")
		}
	})
	r.HandleFunc("/navigate/{url:.+}", func(response http.ResponseWriter, request *http.Request) {
		vars := mux.Vars(request)
		url := vars["url"]
		if chrome != nil {
			errCheck(navigate(chrome, url))
			fmt.Fprintln(response, "Navigated to "+url)
		} else {
			response.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(response, "Chrome disconnected.")
		}
	})
	http.Handle("/", r)
	log.Fatal(http.ListenAndServe(":3001", nil))

}

func getChrome() (*websocket.Conn, error) {
	var tab Tab

	for tab.WebSocketDebuggerUrl == "" {
		// get available tabs and websocket urls from Chrome
		tabs := getTabs()

		if len(tabs) < 1 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		tab = tabs[0]
		wsUrl := tab.WebSocketDebuggerUrl
		// wsUrl = "ws://echo.websocket.org/?encoding=text"
		logger.Println("Connecting to " + wsUrl)
		url, err := url.Parse(wsUrl)
		errCheck(err)

		// Set up websockets connection to Chrome tab
		netConn, err := net.Dial("tcp", host+":"+strconv.Itoa(port))
		errCheck(err)
		chrome, _, err := websocket.NewClient(netConn, url, nil, 2048, 2048)
		errCheck(err)

		// turn "Page" domain on- lets us get navigation notifications
		request, err := encodeClientRequest("Page.enable", nil)
		logger.Println("Request is " + string(request))
		errCheck(err)
		err = chrome.WriteMessage(
			websocket.TextMessage,
			request,
		)
		errCheck(err)

		// turn "Runtime" domain on- lets us execute Javascript in page
		request, err = encodeClientRequest("Runtime.enable", nil)
		logger.Println("Request is " + string(request))
		errCheck(err)
		err = chrome.WriteMessage(
			websocket.TextMessage,
			request,
		)

		// turn "Console" domain on- lets us receive console messages
		request, err = encodeClientRequest("Console.enable", nil)
		errCheck(err)
		err = chrome.WriteMessage(
			websocket.TextMessage,
			request,
		)

		return chrome, err
	}

	return nil, errors.New("Shouldn't have gotten here.")
}

func navigate(chrome *websocket.Conn, url string) error {
	params := map[string]interface{}{
		"url": url,
	}

	request, err := encodeClientRequest("Page.navigate", params)
	errCheck(err)
	logger.Println("Request is " + string(request))
	err = chrome.WriteMessage(
		websocket.TextMessage,
		request,
	)

	return err
}

func getTabs() []Tab {
	url := "http://" + host + ":" + strconv.Itoa(port) + "/json"
	logger.Println("dialing " + url)

	var response bytes.Buffer
	if _, err := ghttp.Get(&response, url); err != nil {
		log.Printf("could not fetch: %v", err)
		return nil
	}

	var tabs []Tab
	err := json.Unmarshal(response.Bytes(), &tabs)
	errCheck(err)

	return tabs
}

func insertJavascript(chrome *websocket.Conn) {
	// Insert growing panes javascript
	params := &RuntimeEvaluateRequestParams{`
		var script = document.createElement('script');
		script.setAttribute('src', '/javascripts/growingpanes.js');
		document.body.appendChild(script);
	`}
	request, err := json.Marshal(&RuntimeEvaluateRequest{
		Method: "Runtime.evaluate",
		Params: params,
		Id:     getRpcId(),
	})
	errCheck(err)
	err = chrome.WriteMessage(
		websocket.TextMessage,
		request,
	)
	errCheck(err)
	logger.Println("Inserted Javascript!!!!!!!!!")
}

func pageDone(chrome *websocket.Conn) {
	err := navigate(chrome, next_url)
	errCheck(err)
}

func errCheck(err error) {
	if err != nil {
		trace := make([]byte, 1024)
		count := runtime.Stack(trace, true)
		logger.Fatalf("%s\nStack of %d bytes: %s", trace, count, err)
	}
}

// Gorilla jsonrpc can provide these methods for us, but they use rand.Int63()
// for the Id and this appears to be too large for chrome to handle for some
// reason.
// clientRequest represents a JSON-RPC request sent by a client.
type clientRequest struct {
	// A String containing the name of the method to be invoked.
	Method string `json:"method"`
	// Object to pass as request parameter to the method.
	Params interface{} `json:"params"`
	// The request id. This can be of any type. It is used to match the
	// response with the request that it is replying to.
	Id int `json:"id"`
}

// EncodeClientRequest encodes parameters for a JSON-RPC client request.
func encodeClientRequest(method string, args interface{}) ([]byte, error) {
	c := &clientRequest{
		Method: method,
		Params: args,
		Id:     getRpcId(),
	}
	return json.Marshal(c)
}

func getRpcId() int {
	return int(rand.Int31())
}

type Watchdog struct {
	name            string
	last            int64
	timeout         int64
	running         bool
	expiredCallback func(*Watchdog)
}

func (w *Watchdog) KeepAlive() {
	atomic.StoreInt64(&w.last, time.Now().UnixNano())
	logger.Println("KeepAlive!")
}

func (w *Watchdog) Start() {
	w.KeepAlive()
	if w.running {
		logger.Println("Watchdog already running, not starting another one.")
		return
	}
	w.running = true
	go func() {
		for {
			time.Sleep(time.Second)
			timeLeft := atomic.LoadInt64(&w.last) - time.Now().Add(-time.Duration(w.timeout)*time.Second).UnixNano()
			timeLeftS := timeLeft / 1000000000
			if (timeLeftS % 5) == 0 {
				logger.Println(strconv.FormatInt(timeLeftS, 10) + "s til " + w.name + " expires")
			}
			if timeLeft < 0 {
				w.expiredCallback(w)
				w.running = false
				logger.Println("Breaking watchdog")
				break
			}
		}
	}()
}
