package cloudwatch

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"sync"
	"time"

	cloudwatchlogsV2 "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
)

type logStreamsType struct {
	groupStreams []string
	sync.RWMutex
}

func (s *logStreamsType) reset(groupStreams []string) {
	s.Lock()
	defer s.Unlock()
	s.groupStreams = groupStreams
}

func (s *logStreamsType) get() []string {
	s.Lock()
	defer s.Unlock()
	return s.groupStreams
}

func params(logGroupName string, streamNames []string,
	startTimeInMillis int64, endTimeInMillis int64,
	grep *string, follow *bool) *cloudwatchlogsV2.FilterLogEventsInput {

	params := &cloudwatchlogsV2.FilterLogEventsInput{
		LogGroupName: &logGroupName,
		StartTime:    &startTimeInMillis}

	if *grep != "" {
		params.FilterPattern = grep
	}

	if streamNames != nil {
		params.LogStreamNames = streamNames
	}

	if !*follow && endTimeInMillis != 0 {
		params.EndTime = &endTimeInMillis
	}
	return params
}

type gs func() ([]string, error)

func initialiseStreams(getStreams gs, retry *bool, idle chan<- bool, logStreams *logStreamsType) error {
	input := make(chan time.Time, 1)
	input <- time.Now()

	for range input {
		s, e := getStreams()
		if e != nil {
			if e.Error() == "ResourceNotFoundException" && *retry {
				log.Println("log group not available. retry in 150 milliseconds.")
				timer := time.After(time.Millisecond * 150)
				input <- <-timer
			} else {
				return e
			}
		} else {
			//found streams, seed them and exit the check loop
			logStreams.reset(s)

			idle <- true
			close(input)
		}
	}
	t := time.NewTicker(time.Second * 5)
	go func() {
		for range t.C {
			s, _ := getStreams()
			// s, _ := getStreams(logGroupName, logStreamName)
			if s != nil {
				logStreams.reset(s)
			}
		}
	}()
	return nil
}

//Tail tails the given stream names in the specified log group name
//To tail all the available streams logStreamName has to be '*'
//It returns a channel where logs line are published
//Unless the follow flag is true the channel is closed once there are no more events available
func Tail(cwlV2 *cloudwatchlogsV2.Client,
	logGroupName *string, logStreamName *string, follow *bool, retry *bool,
	startTime *time.Time, endTime *time.Time,
	grep *string, grepv *string,
	limiter <-chan time.Time, log *log.Logger) (<-chan types.FilteredLogEvent, error) {

	lastSeenTimestamp := startTime.Unix() * 1000
	var endTimeInMillis int64
	if !endTime.IsZero() {
		endTimeInMillis = endTime.Unix() * 1000
	}

	ch := make(chan types.FilteredLogEvent, 1000)
	idle := make(chan bool, 1)

	ttl := 60 * time.Second
	cache := createCache(ttl, defaultPurgeFreq, log)

	logStreams := &logStreamsType{}

	if logStreamName != nil && *logStreamName != "" || *retry {
		getStreams := func(logGroupName *string, logStreamName *string) ([]string, error) {
			var streams []string
			// foundStreams, errCh := LsStreams(cwl, logGroupName, logStreamName)
			foundStreams, errCh := LsStreams(nil, cwlV2, logGroupName, logStreamName)
		outerLoop:
			for {
				select {
				case e := <-errCh:
					return nil, e
				case stream, ok := <-foundStreams:
					if ok {
						streams = append(streams, *stream)
					} else {
						break outerLoop
					}
				case <-time.After(5 * time.Second):
					//TODO better handling of deadlock scenario
				}
			}
			if len(streams) >= 100 { //FilterLogEventPages won't take more than 100 stream names
				start := len(streams) - 100
				streams = streams[start:]
			}
			return streams, nil
		}

		e := initialiseStreams(func() ([]string, error) {
			return getStreams(logGroupName, logStreamName)
		}, retry, idle, logStreams)
		if e != nil {
			return nil, e
		}

		// input := make(chan time.Time, 1)
		// input <- time.Now()

		// for range input {
		// 	s, e := getStreams(logGroupName, logStreamName)
		// 	if e != nil {
		// 		if e.Error() == "ResourceNotFoundException" && *retry {
		// 			log.Println("log group not available. retry in 150 milliseconds.")
		// 			timer := time.After(time.Millisecond * 150)
		// 			input <- <-timer
		// 		} else {
		// 			return nil, e
		// 		}
		// 	} else {
		// 		//found streams, seed them and exit the check loop
		// 		logStreams.reset(s)
		// 		idle <- true
		// 		close(input)
		// 	}
		// }
		// t := time.NewTicker(time.Second * 5)
		// go func() {
		// 	for range t.C {
		// 		s, _ := getStreams(logGroupName, logStreamName)
		// 		if s != nil {
		// 			logStreams.reset(s)
		// 		}
		// 	}
		// }()
	} else {
		idle <- true
	}
	re := regexp.MustCompile(*grepv)
	go func() {
		for range limiter {
			select {
			case <-idle:
				logParam := params(*logGroupName, logStreams.get(), lastSeenTimestamp, endTimeInMillis, grep, follow)
				paginator := cloudwatchlogsV2.NewFilterLogEventsPaginator(cwlV2, logParam)
				for paginator.HasMorePages() {
					res, err := paginator.NextPage(context.TODO())
					if err != nil {
						if err.Error() == "ThrottlingException" {
							log.Printf("Rate exceeded for %s. Wait for 250ms then retry.\n", *logGroupName)

							//Wait and fire request again. 1 Retry allowed.
							time.Sleep(250 * time.Millisecond)
							res, err = paginator.NextPage(context.TODO())
							if err != nil {
								fmt.Fprintln(os.Stderr, err.Error())
								os.Exit(1)
							}
						} else {
							fmt.Fprintln(os.Stderr, err.Error())
							os.Exit(1)
						}
					}
					for _, event := range res.Events {
						if *grepv == "" || !re.MatchString(*event.Message) {
							if !cache.Has(*event.EventId) {
								eventTimestamp := *event.Timestamp

								if eventTimestamp != lastSeenTimestamp {
									if eventTimestamp < lastSeenTimestamp {
										log.Printf("old event:%s, ev-ts:%d, last-ts:%d, cache-size:%d \n", *event.Message, eventTimestamp, lastSeenTimestamp, cache.Size())
									}
									lastSeenTimestamp = eventTimestamp
								}
								cache.Add(*event.EventId, *event.Timestamp)
								ch <- event
							} else {
								log.Printf("%s already seen\n", *event.EventId)
							}
						}
					}

				}
				if !*follow {
					close(ch)
				} else {
					log.Println("last page")
					idle <- true
				}
			case <-time.After(5 * time.Millisecond):
				log.Printf("%s still tailing, Skip polling.\n", *logGroupName)
			}
		}
	}()
	return ch, nil
}
