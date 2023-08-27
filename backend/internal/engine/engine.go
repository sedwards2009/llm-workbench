package engine

import (
	"log"
	"sedwards2009/llm-workbench/internal/data"
	"sedwards2009/llm-workbench/internal/data/responsestatus"
	"sedwards2009/llm-workbench/internal/engine/oobabooga"
	"sedwards2009/llm-workbench/internal/engine/openai"
	"sedwards2009/llm-workbench/internal/engine/types"
)

type Engine struct {
	toWorkerChan      chan *message
	workQueue         []*types.Request
	engineDoneChan    chan bool
	isComputing       bool
	computeWorkerChan chan *types.Request
	models            []*data.Model
	engineBackends    []types.EngineBackend
}

type messageType uint8

const (
	messageType_Enqueue messageType = iota
	messageType_ListModels
	messageType_ScanModels
)

type message struct {
	messageType messageType
	payload     any
}

type listModelsPayload struct {
	out chan *data.ModelOverview
}

type scanModelsPayload struct {
	wait chan bool
}

func NewEngine() *Engine {
	engine := &Engine{
		toWorkerChan:      make(chan *message, 16),
		workQueue:         make([]*types.Request, 0),
		engineDoneChan:    make(chan bool, 16),
		isComputing:       false,
		computeWorkerChan: make(chan *types.Request, 2),
		models:            make([]*data.Model, 0),
	}

	engine.engineBackends = []types.EngineBackend{
		openai.NewEngineBackend(),
		oobabooga.NewEngineBackend(),
	}

	go engine.worker(engine.toWorkerChan)
	return engine
}

func (this *Engine) worker(in chan *message) {
	log.Printf("Engine worker")

	this.scanModels()

	go this.computeWorker(this.computeWorkerChan, this.engineDoneChan)

	for {
		select {
		case message := <-in:
			switch message.messageType {
			case messageType_Enqueue:
				payload := message.payload.(*types.Request)
				log.Printf("engine worker: enqueue %p", payload)
				this.workQueue = append(this.workQueue, payload)
				this.tryNextCompute()

			case messageType_ListModels:
				payload := message.payload.(*listModelsPayload)
				payload.out <- &data.ModelOverview{
					Models: this.models[:],
				}
			case messageType_ScanModels:
				payload := message.payload.(*scanModelsPayload)
				this.scanModels()
				payload.wait <- true
			}

		case <-this.engineDoneChan:
			log.Printf("engine worker: compute done")
			this.isComputing = false
			this.tryNextCompute()
		}
	}
}

func (this *Engine) tryNextCompute() {
	if this.isComputing || len(this.workQueue) == 0 {
		return
	}
	nextWork := this.workQueue[0]

	this.workQueue = this.workQueue[1:]
	this.computeWorkerChan <- nextWork
	this.isComputing = true
}

func (this *Engine) computeWorker(in chan *types.Request, done chan bool) {
	for work := range in {
		this.processWork(work, done)
	}
}

func (this *Engine) processWork(work *types.Request, done chan bool) {
	defer func() {
		done <- true
	}()

	model := this.getModelByID(work.ModelSettings.ModelID)
	if model == nil {
		log.Printf("engine worker: Unable to find model with ID %s\n", work.ModelSettings.ModelID)
		return
	}

	backend := this.getBackendByID(model.Engine)
	if backend == nil {
		log.Printf("engine worker: Unable to find backend with ID %s\n", model.Engine)
		return
	}

	backend.Process(work, model)
}

func (this *Engine) getModelByID(modelID string) *data.Model {
	for _, model := range this.models {
		if model.ID == modelID {
			return model
		}
	}
	return nil
}

func (this *Engine) getBackendByID(backendID string) *types.EngineBackend {
	for _, backend := range this.engineBackends {
		if backend.ID == backendID {
			return &backend
		}
	}
	return nil
}

func (this *Engine) scanModels() {
	allModels := []*data.Model{}
	for _, backend := range this.engineBackends {
		allModels = append(allModels, backend.ScanModels()...)
	}
	this.models = allModels
}

func (this *Engine) Enqueue(messages []data.Message, appendFunc func(string), completeFunc func(),
	setStatusFunc func(responsestatus.ResponseStatus), modelSettings *data.ModelSettings) {

	payload := &types.Request{
		Messages:      messages,
		AppendFunc:    appendFunc,
		CompleteFunc:  completeFunc,
		SetStatusFunc: setStatusFunc,
		ModelSettings: modelSettings,
	}
	message := &message{
		messageType: messageType_Enqueue,
		payload:     payload,
	}
	this.toWorkerChan <- message
}

func (this *Engine) ModelOverview() *data.ModelOverview {
	returnChannel := make(chan *data.ModelOverview)
	this.toWorkerChan <- &message{
		messageType: messageType_ListModels,
		payload:     &listModelsPayload{out: returnChannel},
	}
	return <-returnChannel
}

func (this *Engine) ValidateModelSettings(modelSettings *data.ModelSettings) bool {
	return this.validateModelID(modelSettings.ModelID)
}

func (this *Engine) validateModelID(modelID string) bool {
	models := this.ModelOverview()
	for _, m := range models.Models {
		if m.ID == modelID {
			return true
		}
	}
	return false
}

func (this *Engine) ScanModels() {
	returnChannel := make(chan bool)
	this.toWorkerChan <- &message{
		messageType: messageType_ScanModels,
		payload:     &scanModelsPayload{wait: returnChannel},
	}
	<-returnChannel
}
