package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"sedwards2009/llm-workbench/internal/broadcaster"
	"sedwards2009/llm-workbench/internal/data"
	"sedwards2009/llm-workbench/internal/engine"
	"sedwards2009/llm-workbench/internal/storage"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

var logger gin.HandlerFunc = nil
var sessionStorage *storage.ConcurrentSessionStorage = nil
var llmEngine *engine.Engine = nil
var sessionBroadcaster *broadcaster.Broadcaster = nil

func setupStorage() {
	sessionStorage = storage.NewConcurrentSessionStorage("/home/sbe/devel/llm-workbench/data")
}

func setupEngine() {
	llmEngine = engine.NewEngine()
}

func setupBroadcaster() {
	sessionBroadcaster = broadcaster.NewBroadcaster()
}

func setupRouter() *gin.Engine {
	r := gin.Default()
	logger := gin.Logger()
	r.Use(logger)

	r.Use(cors.New(cors.Config{
		// AllowOrigins:     []string{"*"},
		AllowAllOrigins:  true,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "PATCH"},
		AllowHeaders:     []string{"Origin", "Content-Length", "Content-Type"},
		ExposeHeaders:    []string{"Content-Length", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           10 * time.Second,
	}))

	r.GET("/ping", handlePing)
	r.GET("/session", handleSessionOverview)
	r.POST("/session", handleNewSession)
	r.GET("/session/:sessionId", handleSessionGet)
	r.PUT("/session/:sessionId/prompt", handleSessionPromptPut)
	r.POST("/session/:sessionId/response", handleResponsePost)
	r.GET("/session/:sessionId/changes", handleSessionChangesGet)
	r.DELETE("/session/:sessionId/response/:responseId", handleResponseDelete)

	return r
}

func handlePing(c *gin.Context) {
	c.String(http.StatusOK, "pong")
}

var upgrader = websocket.Upgrader{
	//Solve "request origin not allowed by Upgrader.CheckOrigin"
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

const (
	// Time allowed to write a message to the websocket.
	websocketWriteWait  = 10 * time.Second
	websocketPongWait   = 5 * time.Second
	changeThrottleDelay = 500 * time.Millisecond
	websocketPingPeriod = (websocketPongWait * 9) / 10
)

func handleSessionChangesGet(c *gin.Context) {
	sessionId := c.Params.ByName("sessionId")
	session := sessionStorage.ReadSession(sessionId)
	if session == nil {
		c.String(http.StatusNotFound, "Session not found")
	}

	wsSession, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer wsSession.Close()

	changeChan := make(chan string, 16)
	sessionBroadcaster.Register(sessionId, changeChan)
	pingTicker := time.NewTicker(websocketPingPeriod)
	defer func() {
		sessionBroadcaster.Unregister(changeChan)
		pingTicker.Stop()
		close(changeChan)
	}()

	go websocketReader(wsSession)

	throttleTimer := time.NewTimer(changeThrottleDelay)
	isChangeWaiting := false
	for {
		select {
		case <-changeChan:
			isChangeWaiting = true

		case <-throttleTimer.C:
			if isChangeWaiting {
				isChangeWaiting = false
				wsSession.SetWriteDeadline(time.Now().Add(websocketWriteWait))

				if err := wsSession.WriteMessage(websocket.TextMessage, []byte("changed")); err != nil {
					wsSession.Close()
					if websocket.IsCloseError(err, websocket.CloseGoingAway) {
						log.Printf("Client disconnected for session ID %s.", sessionId)
					} else {
						log.Printf("Writing error for session ID %s: %v.", sessionId, err)
					}
					return
				}
			}
			throttleTimer.Reset(changeThrottleDelay)

		case <-pingTicker.C:
			wsSession.SetWriteDeadline(time.Now().Add(websocketWriteWait))
			if err := wsSession.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				log.Printf("Client disconnected for session ID %s.", sessionId)
				return
			}
		}
	}
}

func websocketReader(ws *websocket.Conn) {
	defer ws.Close()
	ws.SetReadLimit(512)
	ws.SetReadDeadline(time.Now().Add(websocketPongWait))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(websocketPongWait))
		return nil
	})
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			break
		}
	}
}

func handleSessionOverview(c *gin.Context) {
	sessionOverview := sessionStorage.SessionOverview()
	c.JSON(http.StatusOK, sessionOverview)
}

func handleNewSession(c *gin.Context) {
	session := sessionStorage.NewSession()
	if session != nil {
		c.JSON(http.StatusOK, session)
		return
	}
	c.String(http.StatusNotFound, "Session couldn't be created")
}

func handleSessionGet(c *gin.Context) {
	sessionId := c.Params.ByName("sessionId")

	session := sessionStorage.ReadSession(sessionId)
	if session != nil {
		c.JSON(http.StatusOK, session)
		return
	}
	c.String(http.StatusNotFound, "Session not found")
}

func handleSessionPromptPut(c *gin.Context) {
	sessionId := c.Params.ByName("sessionId")
	session := sessionStorage.ReadSession(sessionId)
	if session == nil {
		c.String(http.StatusNotFound, "Session not found")
		return
	}

	var data struct {
		Prompt string `json:"prompt"`
	}

	if err := c.ShouldBindJSON(&data); err != nil {
		c.String(http.StatusBadRequest, "Couldn't parse the JSON PUT body.")
		return
	}

	session.Prompt = data.Prompt
	sessionStorage.WriteSession(session)

	c.JSON(http.StatusOK, session)
}

func handleResponsePost(c *gin.Context) {
	sessionId := c.Params.ByName("sessionId")
	session := sessionStorage.ReadSession(sessionId)
	if session == nil {
		c.String(http.StatusNotFound, "Session not found")
		return
	}

	response, err := sessionStorage.NewResponse(sessionId)
	if err != nil {
		c.String(http.StatusInternalServerError, fmt.Sprintf("Error occured while creating new response: %v", err))
		return
	}

	responseId := response.ID
	appendFunc := func(text string) {
		sessionStorage.AppendToResponse(sessionId, responseId, text)
		sessionBroadcaster.Send(sessionId, "changed")
	}

	completeFunc := func() {
		sessionBroadcaster.Send(sessionId, "changed")
	}

	setStatusFunc := func(status data.ResponseStatus) {
		sessionStorage.SetResponseStatus(sessionId, responseId, status)
		sessionBroadcaster.Send(sessionId, "changed")
	}

	llmEngine.Enqueue(response.Prompt, appendFunc, completeFunc, setStatusFunc)
	c.JSON(http.StatusOK, response)
}

func handleResponseDelete(c *gin.Context) {
	sessionId := c.Params.ByName("sessionId")
	responseId := c.Params.ByName("responseId")

	err := sessionStorage.DeleteResponse(sessionId, responseId)
	if err != nil {
		c.String(http.StatusInternalServerError, fmt.Sprintf("Error occured while deleting response: %v", err))
		return
	}

	c.Status(http.StatusNoContent)
}

func main() {
	err := godotenv.Load(".env")
	if err != nil {
		fmt.Println("Error loading .env file.")
	}

	// parsedArgs, errorString := argsparser.Parse(&os.Args)
	setupStorage()
	setupEngine()
	setupBroadcaster()
	r := setupRouter()
	r.Run(":8080")
}
