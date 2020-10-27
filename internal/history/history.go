package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Item struct {
	ID         string
	Group      string
	Name       string
	Type       string
	Output     []byte
	CreatedAt  time.Time
	Duration   time.Duration
	Metric     int64
	MetricUnit string
	Status     string
	Error      string
}

func (item Item) String() string {
	return strings.Join([]string{
		fmt.Sprintf("Item{"),
		fmt.Sprintf("\tGroup: %s,", item.Group),
		fmt.Sprintf("\tName: %s,", item.Name),
		fmt.Sprintf("\tType: %s,", item.Type),
		fmt.Sprintf("\tOutput: '%s',", strings.Join(strings.Split(string(item.Output), "\n"), "\\n")),
		fmt.Sprintf("\tCreatedAt: %s,", item.CreatedAt),
		fmt.Sprintf("\tMetric: %d%s,", item.Metric, item.MetricUnit),
		fmt.Sprintf("\tStatus: %s,", item.Status),
		fmt.Sprintf("\tError: '%s',", item.Error),
		fmt.Sprintf("}"),
	}, "\n")
}

func (item Item) writeTo(out io.Writer) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	n, err := out.Write(append(data, '\n'))
	if n < len(data) {
		return fmt.Errorf("Wrote partial data (error: %s)", err)
	}
	return err
}

func sendError(receivers []writeRequest, err error) {
	for _, recv := range receivers {
		recv.errChan <- err
	}
}

type writeRequest struct {
	item    Item
	errChan chan error
}

type listNode struct {
	value Item
	next  *listNode
	prev  *listNode
}

func (ln *listNode) String() string {
	return ln.value.String()
}

type dataContainer struct {
	byID map[string]*listNode
	head *listNode
	tail *listNode
}

type File struct {
	fd         *os.File
	writes     chan writeRequest
	writerWg   *sync.WaitGroup
	done       chan bool
	data       map[string]*dataContainer
	rwMux      *sync.RWMutex
	maxEntries int
	logger     *log.Logger
}

type NewOptions struct {
	File                string
	MaxEntries          int
	MaxConcurrentWrites int
}

func New(options NewOptions) (*File, error) {
	fd, err := os.OpenFile(
		options.File,
		os.O_RDWR|os.O_CREATE,
		0755,
	)
	if err != nil {
		return nil, err
	}

	file := &File{
		fd:         fd,
		writes:     make(chan writeRequest, options.MaxConcurrentWrites),
		writerWg:   &sync.WaitGroup{},
		done:       make(chan bool),
		data:       map[string]*dataContainer{},
		rwMux:      &sync.RWMutex{},
		maxEntries: options.MaxEntries,
		logger:     log.New(os.Stdout, "history: ", log.LstdFlags|log.Lmsgprefix),
	}
	file.logger.Printf("Opened history file: %s", options.File)

	bufferedReader := bufio.NewReader(fd)
	var item Item
	var line []byte
	for err != io.EOF {
		line, err = bufferedReader.ReadBytes('\n')
		if len(line) > 0 {
			if err := json.Unmarshal(line[:len(line)-1], &item); err != nil {
				return nil, err
			}
			file.addItem(item)
		}
	}

	if len(file.data) > 0 {
		// Re-initialize the AOF
		writeBuffer := &bytes.Buffer{}
		for _, container := range file.data {
			for curr := container.head; curr != nil; curr = curr.next {
				if err := curr.value.writeTo(writeBuffer); err != nil {
					return nil, err
				}
			}
		}

		if err := file.fd.Truncate(0); err != nil {
			return nil, err
		}
		if _, err := file.fd.Seek(0, 0); err != nil {
			return nil, err
		}
		if _, err := io.Copy(file.fd, writeBuffer); err != nil {
			return nil, err
		}
		if err := file.fd.Sync(); err != nil {
			return nil, err
		}
	}

	numItems := 0
	for _, group := range file.data {
		numItems += len(group.byID)
	}
	if numItems > 0 {
		file.logger.Printf("Imported %d groups and %d items from history", len(file.data), numItems)
	}

	file.writerWg.Add(1)
	go file.bgWriter()
	return file, nil
}

func (file *File) bgWriter() {
	defer file.writerWg.Done()

	for {
		select {
		case req := <-file.writes:
			file.rwMux.Lock()
			records := make([]writeRequest, 1)
			records[0] = req

			// TODO: Instead of panicing, it should perform a
			// rollback on the 'data' state

			buffer := &bytes.Buffer{}
			req.item = file.addItem(req.item)
			if err := req.item.writeTo(buffer); err != nil {
				sendError(records, err)
			} else {
				collect := true
				var err error

				for collect && err != nil {
					select {
					case r := <-file.writes:
						records = append(records, r)
						r.item = file.addItem(r.item)
						err = r.item.writeTo(buffer)
					default:
						collect = false
					}
				}

				if err != nil {
					sendError(records, err)
				} else if n, err := io.Copy(file.fd, buffer); err != nil {
					file.rwMux.Unlock()
					panic(err)
				} else if n < int64(buffer.Len()) {
					file.rwMux.Unlock()
					panic(fmt.Errorf("Wrote only %d bytes to file", n))
				} else {
					file.logger.Printf("Wrote %d records", len(records))
					file.rwMux.Unlock()
					sendError(records, nil)
				}

				if err := file.fd.Sync(); err != nil {
					file.logger.Printf("Warning: fsync failed: %s", err)
				}
			}

		case <-file.done:
			file.logger.Printf("Closing history file")
			return
		}
	}
}

func (file *File) addItem(item Item) Item {
	if _, ok := file.data[item.Group]; !ok {
		file.data[item.Group] = &dataContainer{
			byID: make(map[string]*listNode, 100),
		}
	}
	container := file.data[item.Group]

	if item.Type == "boolean" {
		item.ID = fmt.Sprintf("%s|%s|%d|0", item.Group, item.Name, item.CreatedAt.UTC().UnixNano()/int64(24*time.Hour))
	} else {
		n := int64(0)
		prefix := fmt.Sprintf("%s|%s|%d|", item.Group, item.Name, item.CreatedAt.UTC().UnixNano())
		for {
			item.ID = prefix + strconv.FormatInt(n, 10)
			if _, exists := container.byID[item.ID]; !exists {
				break
			}
			n++
		}
	}

	node, exists := container.byID[item.ID]
	if !exists {
		node = &listNode{}
		container.byID[item.ID] = node
	}
	node.value = item

	if item.Type == "metric" || !exists {
		file.logger.Printf("Inserting (size = %d): %s", len(container.byID), item)

		if container.head == nil {
			container.head = node
			container.tail = node
		} else {
			inserted := false
			var prev *listNode

			for curr := container.head; curr != nil && !inserted; prev, curr = curr, curr.next {
				if !node.value.CreatedAt.Before(curr.value.CreatedAt) {
					if prev == nil {
						node.next = container.head
						container.head.prev = node
						container.head = node
					} else {
						node.prev = prev
						node.next = prev.next
						prev.next = node
					}
					inserted = true
				}
			}

			if !inserted {
				container.tail.next = node
				node.prev = container.tail
				container.tail = node
			}

			for len(container.byID) > file.maxEntries {
				drop := container.tail
				file.logger.Printf("Dropping old item: %s", drop.value)
				container.tail = drop.prev
				if container.tail == nil {
					container.head = nil
				} else {
					container.tail.next = nil
				}
				delete(container.byID, drop.value.ID)
			}
		}
	} else {
		file.logger.Printf("Replacing: %s", item)
	}

	return item
}

func (file *File) Append(item Item) error {
	errChan := make(chan error)
	fmt.Printf("adding to write queue: %s\n", item.Output)
	file.writes <- writeRequest{
		item:    item,
		errChan: errChan,
	}
	return <-errChan

	// file.rwMux.Lock()
	// err := file.addItem(item).writeTo(file.fd)
	// file.rwMux.Unlock()
	// return err
}

func (file *File) GetGroups() []string {
	keys := make([]string, len(file.data))
	idx := 0

	file.rwMux.RLock()
	for key, _ := range file.data {
		keys[idx] = key
		idx++
	}
	file.rwMux.RUnlock()

	return keys
}

func (file *File) GetGroupItems(group string) []Item {
	file.rwMux.RLock()
	container, _ := file.data[group]
	file.rwMux.RUnlock()

	list := make([]Item, 0, len(container.byID))
	for curr := container.head; curr != nil; curr = curr.next {
		list = append(list, curr.value)
	}
	return list
}

func (file *File) Close() {
	close(file.done)
	file.writerWg.Wait()
}