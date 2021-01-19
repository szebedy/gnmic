package kafka_input

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/karimra/gnmic/formatters"
	"github.com/karimra/gnmic/inputs"
	"github.com/karimra/gnmic/outputs"
	"google.golang.org/protobuf/proto"
)

const (
	loggingPrefix         = "kafka_input "
	defaultFormat         = "event"
	defaultTopic          = "telemetry"
	defaultNumWorkers     = 1
	defaultSessionTimeout = 10 * time.Second
	defaultAddress        = "localhost:9092"
	defaultGroupID        = "gnmic-consumers"
)

var defaultVersion = sarama.V2_5_0_0

func init() {
	inputs.Register("kafka", func() inputs.Input {
		return &KafkaInput{
			Cfg:    &Config{},
			logger: log.New(ioutil.Discard, loggingPrefix, log.LstdFlags|log.Lmicroseconds),
			wg:     new(sync.WaitGroup),
		}
	})
}

// KafkaInput //
type KafkaInput struct {
	Cfg     *Config
	cfn     context.CancelFunc
	logger  sarama.StdLogger
	wg      *sync.WaitGroup
	outputs []outputs.Output
}

// Config //
type Config struct {
	Name             string        `mapstructure:"name,omitempty"`
	Address          string        `mapstructure:"address,omitempty"`
	Topic            string        `mapstructure:"topic,omitempty"`
	GroupID          string        `mapstructure:"group-id,omitempty"`
	MaxRetry         int           `mapstructure:"max-retry,omitempty"`
	Timeout          time.Duration `mapstructure:"timeout,omitempty"`
	RecoveryWaitTime time.Duration `mapstructure:"recovery-wait-time,omitempty"`
	Version          string        `mapstructure:"version,omitempty"`
	Format           string        `mapstructure:"format,omitempty"`
	Debug            bool          `mapstructure:"debug,omitempty"`
	NumWorkers       int           `mapstructure:"num-workers,omitempty"`
	Outputs          []string      `mapstructure:"outputs,omitempty"`

	kafkaVersion sarama.KafkaVersion
}

func (k *KafkaInput) Start(ctx context.Context, cfg map[string]interface{}, opts ...inputs.Option) error {
	err := outputs.DecodeConfig(cfg, k.Cfg)
	if err != nil {
		return err
	}
	err = k.setDefaults()
	if err != nil {
		return err
	}
	for _, opt := range opts {
		opt(k)
	}
	k.wg.Add(k.Cfg.NumWorkers)
	for i := 0; i < k.Cfg.NumWorkers; i++ {
		go k.worker(ctx, i)
	}
	return nil
}

func (k *KafkaInput) worker(ctx context.Context, idx int) {
	defer k.wg.Done()
	config := sarama.NewConfig()
	config.Version = sarama.V0_11_0_1
	config.ClientID = fmt.Sprintf("%s-%d", k.Cfg.Name, idx)
	config.Consumer.Return.Errors = true
	config.Consumer.Group.Session.Timeout = k.Cfg.Timeout
	config.Consumer.Group.Rebalance.Strategy = sarama.BalanceStrategyRange
	// TODO: finish kafka config

	workerLogPrefix := fmt.Sprintf("worker-%d", idx)
START:
	k.logger.Printf("%s starting consumer group %s", workerLogPrefix, k.Cfg.GroupID)
	consumerGrp, err := sarama.NewConsumerGroup(strings.Split(k.Cfg.Address, ","), k.Cfg.GroupID, config)
	if err != nil {
		k.logger.Printf("%s failed to create consumer group: %v", workerLogPrefix, err)
		time.Sleep(k.Cfg.RecoveryWaitTime)
		goto START
	}
	k.logger.Printf("%s started consumer group %s", workerLogPrefix, k.Cfg.GroupID)
	defer consumerGrp.Close()
	cons := &consumer{
		ready:   make(chan bool),
		msgChan: make(chan *sarama.ConsumerMessage),
	}
	go func() {
		var err error
		for {
			if ctx.Err() != nil {
				return
			}
			err = consumerGrp.Consume(ctx, strings.Split(k.Cfg.Topic, ","), cons)
			if err != nil {
				if k.Cfg.Debug {
					k.logger.Printf("%s failed to start consumer, topics=%q, group=%q : %v", workerLogPrefix, k.Cfg.Topic, k.Cfg.GroupID, err)
				}
				continue
			}
			cons.ready = make(chan bool)
		}
	}()
	<-cons.ready
	k.logger.Printf("%s kafka consumer ready", workerLogPrefix)
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-cons.msgChan:
			if k.Cfg.Debug {
				k.logger.Printf("%s client=%s received msg, topic=%s, partition=%d, key=%q, value=%s", workerLogPrefix, config.ClientID, m.Topic, m.Partition, string(m.Key), string(m.Value))
			}
			switch k.Cfg.Format {
			case "event":
				evMsgs := make([]*formatters.EventMsg, 1)
				err = json.Unmarshal(m.Value, &evMsgs)
				if err != nil {
					if k.Cfg.Debug {
						k.logger.Printf("%s failed to unmarshal event msg: %v", workerLogPrefix, err)
					}
					continue
				}
				go func() {
					for _, o := range k.outputs {
						for _, ev := range evMsgs {
							o.WriteEvent(ctx, ev)
						}
					}
				}()
			case "proto":
				var protoMsg proto.Message
				err = proto.Unmarshal(m.Value, protoMsg)
				if err != nil {
					if k.Cfg.Debug {
						k.logger.Printf("%s failed to unmarshal proto msg: %v", workerLogPrefix, err)
					}
					continue
				}
				meta := outputs.Meta{}
				go func() {
					for _, o := range k.outputs {
						o.Write(ctx, protoMsg, meta)
					}
				}()
			}
		case err := <-consumerGrp.Errors():
			k.logger.Printf("%s client=%s, consumer-group=%s error: %v", workerLogPrefix, config.ClientID, k.Cfg.GroupID, err)
			time.Sleep(k.Cfg.RecoveryWaitTime)
			goto START
		}
	}
}

func (k *KafkaInput) Close() error {
	k.cfn()
	k.wg.Wait()
	return nil
}

func (k *KafkaInput) SetLogger(logger *log.Logger) {
	if logger != nil {
		sarama.Logger = log.New(logger.Writer(), loggingPrefix, logger.Flags())
		k.logger = sarama.Logger
	}
}

func (k *KafkaInput) SetOutputs(outs map[string]outputs.Output) {
	if len(k.Cfg.Outputs) == 0 {
		for _, o := range outs {
			k.outputs = append(k.outputs, o)
		}
		return
	}
	for _, name := range k.Cfg.Outputs {
		if o, ok := outs[name]; ok {
			k.outputs = append(k.outputs, o)
		}
	}
}

// helper funcs

func (k *KafkaInput) setDefaults() error {
	var err error
	if k.Cfg.Version != "" {
		k.Cfg.kafkaVersion, err = sarama.ParseKafkaVersion(k.Cfg.Version)
		if err != nil {
			return err
		}
	} else {
		k.Cfg.kafkaVersion = defaultVersion

	}
	if k.Cfg.Format == "" {
		k.Cfg.Format = defaultFormat
	}
	if !(strings.ToLower(k.Cfg.Format) == "event" || strings.ToLower(k.Cfg.Format) == "proto") {
		return fmt.Errorf("unsupported input format")
	}
	if k.Cfg.Topic == "" {
		k.Cfg.Topic = defaultTopic
	}
	if k.Cfg.Address == "" {
		k.Cfg.Address = defaultAddress
	}
	if k.Cfg.NumWorkers <= 0 {
		k.Cfg.NumWorkers = defaultNumWorkers
	}
	if k.Cfg.Timeout <= 2*time.Millisecond {
		k.Cfg.Timeout = defaultSessionTimeout
	}
	if k.Cfg.GroupID == "" {
		k.Cfg.GroupID = defaultGroupID
	}
	return nil
}

// consumer
// ref: https://github.com/Shopify/sarama/blob/master/examples/consumergroup/main.go
// consumer represents a Sarama consumer group consumer
type consumer struct {
	ready   chan bool
	msgChan chan *sarama.ConsumerMessage
}

// Setup is run at the beginning of a new session, before ConsumeClaim
func (consumer *consumer) Setup(sarama.ConsumerGroupSession) error {
	// Mark the consumer as ready
	close(consumer.ready)
	return nil
}

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have exited
func (consumer *consumer) Cleanup(sarama.ConsumerGroupSession) error {
	return nil
}

// ConsumeClaim must start a consumer loop of ConsumerGroupClaim's Messages().
func (consumer *consumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for message := range claim.Messages() {
		consumer.msgChan <- message
		session.MarkMessage(message, "")
	}
	return nil
}
