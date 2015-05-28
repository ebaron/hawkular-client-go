package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TODO: Add statistics support? Error metrics (connection errors, request errors), request metrics:
// totals, request rate? mean time, avg time, max, min, percentiles etc.. ? And clear metrics..
// Stateful metrics? Kinda like Counters.Inc(..) ?

// More detailed error

type HawkularClientError struct {
	msg  string
	Code int
}

func (self *HawkularClientError) Error() string {
	return fmt.Sprintf("Hawkular returned status code %d, error message: %s", self.Code, self.msg)
}

// Client creation and instance config

const (
	base_url string = "hawkular/metrics"
)

type Parameters struct {
	Tenant string
	Host   string
	Path   string // Optional
}

type Client struct {
	Tenant  string
	Baseurl string
	client  *http.Client
}

func NewHawkularClient(p Parameters) (*Client, error) {
	if p.Path == "" {
		p.Path = base_url
	}

	url := fmt.Sprintf("http://%s/%s", p.Host, p.Path) // TODO use *net.URL?
	return &Client{
		Baseurl: url,
		Tenant:  p.Tenant,
		client:  &http.Client{},
	}, nil
}

// Public functions

// Creates a new metric, and returns true if creation succeeded, false if not (metric was already created).
// err is returned only in case of another error than 'metric already created'
func (self *Client) Create(md MetricDefinition) (bool, error) {
	jsonb, err := json.Marshal(&md)
	if err != nil {
		return false, err
	}
	err = self.post(self.metricsUrl(md.Type), jsonb)
	if err, ok := err.(*HawkularClientError); ok {
		if err.Code != http.StatusConflict {
			return false, err
		} else {
			return false, nil
		}
	}
	return true, nil

}

// Fetch metric definitions for one metric type
func (self *Client) Definitions(t MetricType) ([]*MetricDefinition, error) {
	q := make(map[string]string)
	q["type"] = t.shortForm()
	url, err := self.paramUrl(self.metricsUrl(Generic), q)
	if err != nil {
		return nil, err
	}
	b, err := self.get(url)
	if err != nil {
		return nil, err
	}

	md := []*MetricDefinition{}
	if b != nil {
		if err = json.Unmarshal(b, &md); err != nil {
			return nil, err
		}
	}

	for _, m := range md {
		m.Type = t
	}

	return md, nil
}

// Return a single definition
func (self *Client) Definition(t MetricType, id string) (*MetricDefinition, error) {
	url := self.singleMetricsUrl(t, id)

	b, err := self.get(url)
	if err != nil {
		return nil, err
	}

	md := MetricDefinition{}
	if b != nil {
		if err = json.Unmarshal(b, &md); err != nil {
			return nil, err
		}
	}
	md.Type = t
	return &md, nil
}

// Fetch metric definition tags
func (self *Client) Tags(t MetricType, id string) (*map[string]string, error) {
	b, err := self.get(self.tagsUrl(t, id))
	if err != nil {
		return nil, err
	}

	tags := make(map[string]string)
	// Repetive code.. clean up with other queries to somewhere..
	if b != nil {
		if err = json.Unmarshal(b, &tags); err != nil {
			return nil, err
		}
	}

	return &tags, nil
}

// Replace metric definition tags
// TODO: Should this be "ReplaceTags" etc?
func (self *Client) UpdateTags(t MetricType, id string, tags map[string]string) error {
	b, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	return self.put(self.tagsUrl(t, id), b)
}

// Delete given tags from the definition
func (self *Client) DeleteTags(t MetricType, id string, deleted map[string]string) error {
	tags := make([]string, 0, len(deleted))
	for k, v := range deleted {
		tags = append(tags, fmt.Sprintf("%s:%s", k, v))
	}
	j := strings.Join(tags, ",")
	url := fmt.Sprintf("%s/%s", self.tagsUrl(t, id), j)
	return self.del(url)
}

// Take input of single Metric instance. If Timestamp is not defined, use current time
func (self *Client) PushSingleGaugeMetric(id string, m Datapoint) error {

	if _, ok := m.Value.(float64); !ok {
		f, err := ConvertToFloat64(m.Value)
		if err != nil {
			return err
		}
		m.Value = f
	}

	if m.Timestamp == 0 {
		m.Timestamp = UnixMilli(time.Now())
	}

	mH := MetricHeader{
		Id:   id,
		Data: []Datapoint{m},
		Type: Gauge,
	}
	return self.Write([]MetricHeader{mH})
}

// Read single Gauge metric's datapoints.
// TODO: Remove and replace with better Read properties? Perhaps with iterators?
func (self *Client) SingleGaugeMetric(id string, options map[string]string) ([]*Datapoint, error) {
	url, err := self.paramUrl(self.dataUrl(self.singleMetricsUrl(Gauge, id)), options)
	if err != nil {
		return nil, err
	}
	b, err := self.get(url)
	if err != nil {
		return nil, err
	}
	metrics := []*Datapoint{}

	if b != nil {
		if err = json.Unmarshal(b, &metrics); err != nil {
			return nil, err
		}
	}

	return metrics, nil

}

// func (self *Client) QueryGaugesWithTags(id string, tags map[string]string) ([]MetricDefinition, error) {

// Write using mixedmultimetrics
// For now supports only single metricType per request
func (self *Client) Write(metrics []MetricHeader) error {
	if len(metrics) > 0 {
		metricType := metrics[0].Type // Temp solution
		if err := metricType.validate(); err != nil {
			return err
		}

		jsonb, err := json.Marshal(&metrics)
		if err != nil {
			return err
		}
		return self.post(self.dataUrl(self.metricsUrl(metricType)), jsonb)
	}
	return nil
}

// HTTP Helper functions

func (self *Client) get(url string) ([]byte, error) {
	return self.send(url, "GET", nil)
}

func (self *Client) post(url string, json []byte) error {
	_, err := self.send(url, "POST", json)
	return err
}

func (self *Client) put(url string, json []byte) error {
	_, err := self.send(url, "PUT", json)
	return err
}

func (self *Client) del(url string) error {
	_, err := self.send(url, "DELETE", nil)
	return err
}

func (self *Client) send(url string, method string, json []byte) ([]byte, error) {
	req, _ := http.NewRequest(method, url, bytes.NewBuffer(json))
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Hawkular-Tenant", self.Tenant)
	resp, err := self.client.Do(req)

	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		b, err := ioutil.ReadAll(resp.Body)
		return b, err
	} else if resp.StatusCode > 399 {
		return nil, self.parseErrorResponse(resp)
	} else {
		return nil, nil // Nothing to answer..
	}
}

func (self *Client) parseErrorResponse(resp *http.Response) error {
	// Parse error messages here correctly..
	reply, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return &HawkularClientError{Code: resp.StatusCode,
			msg: fmt.Sprintf("Reply could not be parsed: %s", err.Error()),
		}
	}

	details := &HawkularError{}

	err = json.Unmarshal(reply, details)
	if err != nil {
		return &HawkularClientError{Code: resp.StatusCode,
			msg: fmt.Sprintf("Reply could not be parsed: %s", err.Error()),
		}
	}

	return &HawkularClientError{Code: resp.StatusCode,
		msg: details.ErrorMsg,
	}
}

// URL functions (...)

func (self *Client) metricsUrl(metricType MetricType) string {
	return fmt.Sprintf("%s/%s", self.Baseurl, metricType.String())
}

func (self *Client) singleMetricsUrl(metricType MetricType, id string) string {
	return fmt.Sprintf("%s/%s", self.metricsUrl(metricType), id)
}

func (self *Client) tagsUrl(mt MetricType, id string) string {
	return fmt.Sprintf("%s/tags", self.singleMetricsUrl(mt, id))
}

func (self *Client) dataUrl(url string) string {
	return fmt.Sprintf("%s/data", url)
}

func (self *Client) paramUrl(starturl string, options map[string]string) (string, error) {
	u, err := url.Parse(starturl)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, v := range options {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
