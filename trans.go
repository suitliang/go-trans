package go_trans

import (
	"github.com/tangs-drm/go-trans/log"
	"github.com/tangs-drm/go-trans/util"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type TransPlugin interface {
	// Return the type of the transcode plug-in
	Type() string

	// Start the transcoding task.
	//
	// input: Input file name.
	// output: Output file name.
	// args: The parameters of the transcoding execution, such as
	//		{"-b:v": 1200000, "-r": 30}.
	//
	// int: Status code, see error.go for detail.
	// TransMessage: The output information of the transcoding,
	// 		including the printing information of the transcoding success
	// 		and the failure of the transcoding.
	// error: NewError information of the system.
	Exec(input, output string, args map[string]interface{}) (int, TransMessage, error)

	// Cancel the current transcoding task.
	// error: error message.
	Cancel() error

	// Progress return the current transcoding progress.
	//
	// map[string]interface{}:
	// error: error message.
	Progress() (map[string]interface{}, error)
}

const (
	TransRunning  = "Running"
	TransError    = "Error"
	TransCancel   = "Cancel"
	TransNotStart = "Not Start"
	TransSuccess  = "Success"
)

// Transcoding task scheduler
type TransManage struct {
	// Maximum number of transcoding threads.
	MaxRunningNum int
	// The number of transcoding threads that are currently running.
	CurrentRunning int

	// Formats of transcoding support
	Formats []string
	//
	TransPlugin map[string]func() TransPlugin
	// Transcoding task list
	Tasks []*Task

	// Transcode callback error retry times.
	TryTimes int
	Status   string

	// Callback address after transcoding
	Address string

	sign chan int
	lock *sync.Mutex
}

// The default number of transcoding threads
var DefaultMaxRunningNum = 1

var DefaultTryTimes = 1
var DefaultFormats = []string{"flv"}

// The default trans manager.
var DefaultTransManager = &TransManage{
	MaxRunningNum:  DefaultMaxRunningNum,
	CurrentRunning: 0,
	Formats:        DefaultFormats,
	TransPlugin:    map[string]func() TransPlugin{},
	Tasks:          []*Task{},
	TryTimes:       DefaultTryTimes,
	Status:         TransNotStart,
	sign:           make(chan int, 256),
	lock:           &sync.Mutex{},
}

// Registering a supported transcode format with the transPlugin.
//
// format: video format like .flv, .avi.
// transPlugin: transcoding plugin.
//
// error: error message.
func (tm *TransManage) RegisterPlugin(format string, plugin func() TransPlugin) {
	tm.TransPlugin[format] = plugin
	for _, format := range tm.Formats {
		if format == format {
			return
		}
	}
	tm.Formats = append(tm.Formats, format)
}

func RegisterPlugin(format string, plugin func() TransPlugin) {
	DefaultTransManager.RegisterPlugin(format, plugin)
}

// GetFormats return the supported transcoding format
func (tm *TransManage) GetFormats() []string {
	return tm.Formats
}

func GetFormats() []string {
	return DefaultTransManager.GetFormats()
}

// SetMaxRunningNum set the maximum number of transcoding threads.This method
// is called if the call needs to be executed before method TransManage.Run().
func (tm *TransManage) SetMaxRunningNum(num int) {
	tm.MaxRunningNum = num
}

func SetMaxRunningNum(num int) {
	DefaultTransManager.SetMaxRunningNum(num)
}

// Set callback address. like http://callback.example.com/callback
func SetCallbackAddress(addr string) {
	DefaultTransManager.SetCallbackAddress(addr)
}

func (tm *TransManage) SetCallbackAddress(addr string) {
	tm.Address = addr
}

// AddTask add a transcoding task, but just add the transcoding queue at this time,
// and do not really start transcoding.
//
// input: Input filename.
// output: Output filename.
func (tm *TransManage) AddTask(input, output string) (Task, error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	// check input and output
	var inputExt = filepath.Ext(input)
	var outputExt = filepath.Ext(output)
	var err error

	if "" == inputExt {
		err = util.NewError("input is invalid: %v", input)
		log.E("AddTask error with input: %v", err)
		return Task{}, err
	}
	if "" == outputExt {
		err = util.NewError("output is invalid: %v", output)
		log.E("AddTask error with output: %v", err)
		return Task{}, err
	}
	var plugin = tm.TransPlugin[inputExt]
	if plugin == nil {
		err = util.NewError("unsupported format: %v", inputExt)
		log.E("AddTask error with format: %v", err)
		return Task{}, err
	}
	var task = &Task{
		Id:     util.UUID(),
		Input:  input,
		Output: output,
		Plugin: plugin(),
	}

	// todo. save into database.
	tm.Tasks = append(tm.Tasks, task)

	tm.sign <- 1

	return *task, nil
}

func RunTask() {
	go DefaultTransManager.runTask()
}

func (tm *TransManage) runTask() {
	defer func() {
		if err := recover(); err != nil {
		}
	}()

	for {
		<-tm.sign
		if tm.CurrentRunning >= tm.MaxRunningNum {
			continue
		}

		for _, task := range tm.Tasks {
			if TASK_WAITING == task.Status {
				continue
			}
			go tm.exec(task)
		}
	}
}

func (tm *TransManage) exec(task *Task) {
	task.Status = TransRunning
	code, result, err1 := task.Plugin.Exec(task.Input, task.Output, task.Args)
	call := Call{
		Code:         code,
		Error:        ErrorCode[code],
		ErrorMessage: err1,
		Task:         *task,
		Message:      result,
	}
	if err1 != nil {
		log.E("TransManage exec task: %v complete with code %v, err %v", util.S2Json(task), code, err1)
		task.Status = TransError
	} else {
		log.D("TransManage exec task: %v complete with result: %v", util.S2Json(task), util.S2Json(result))
		task.Status = TransSuccess
	}
	err2 := tm.CallBack(call)
	if err2 != nil {
		log.E("TransManage exec task: %v complete but error with callback: %v, error: %v", util.S2Json(task), util.S2Json(call), err2)
	} else {
		log.D("TransManage exec task: %v complete and callback success")
	}
	tm.sign <- 1

	tm.lock.Lock()
	tm.popTask(task.Id)
	tm.lock.Unlock()
}

func (tm *TransManage) popTask(taskId string) error {
	for index, task := range tm.Tasks {
		if task.Id != taskId {
			continue
		}

		if 0 == index {
			tm.Tasks = tm.Tasks[1:]
			return nil
		}

		var length = len(tm.Tasks)
		if length-1 == index {
			tm.Tasks = tm.Tasks[:length-1]
			return nil
		}

		var tasks = append(tm.Tasks)
		tm.Tasks = tasks[0:index]
		tm.Tasks = append(tm.Tasks, tasks[index+1:]...)
		return nil

	}
	return util.NewError("%v", TransNotFound)
}

// ListTask list the transcoding task.
//
// limit: Maximum number tasks return when func exec. If limit is less than 0, all of the task data is returned.
// skip: List start from skip.
//
// []Task: Tasks' detail.
// int: The count of all tasks.
func (tm *TransManage) ListTask(limit, skip int) ([]Task, int) {
	return nil, 0
}

// Cancel the transcoding process by taskId.
// It will return error TransNotFound if can't find task.
// todo. If exec Callback here?
func (tm *TransManage) Cancel(taskId string) error {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	for _, task := range tm.Tasks {
		if task.Id != taskId {
			continue
		}

		var err = task.Plugin.Cancel()
		if err != nil {
			return err
		}
		task.Status = TransCancel
		tm.popTask(taskId)
		return nil
	}
	return util.NewError("%v", TransNotFound)
}

func (tm *TransManage) Process(id []string) {

}

func (tm *TransManage) CallBack(call Call) error {
	if "" == tm.Address {
		log.W("CallBack will return because of empty address")
		return nil
	}

	for i := 0; i < tm.TryTimes; i++ {
		resp, err := http.Post(tm.Address, "application/json", strings.NewReader(util.S2Json(call)))
		if err != nil {
			log.W("CallBack with retryTime: %v, address: %v, call: %v error: %v", i, tm.Address, util.S2Json(call), err)
			duration := time.Duration(rand.Intn(10)+10) * time.Second
			time.Sleep(duration)
			continue
		}
		if http.StatusOK != resp.StatusCode {
			log.W("CallBack with retryTime: %v, address: %v, call: %v code: %v", i, tm.Address, util.S2Json(call), resp.StatusCode)
			duration := time.Duration(rand.Intn(10)+10) * time.Second
			time.Sleep(duration)
			continue
		}
		log.W("CallBack with retryTime: %v, address: %v, call: %v success", i, tm.Address, util.S2Json(call))
		return nil
	}
	return util.NewError("%v", TransTooManyTimes)
}
