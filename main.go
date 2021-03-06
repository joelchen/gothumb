package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/DAddYE/vips"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/julienschmidt/httprouter"
	"github.com/spf13/viper"
	"golang.org/x/crypto/sha3"
)

var (
	port       int
	bucket     string
	httpClient = http.DefaultClient
)

// Size in bytes
const (
	_  = iota
	KB = 1 << (10 * iota)
	MB
)

func main() {
	viper.SetConfigName("config")
	viper.AddConfigPath(".")
	log.SetFlags(0)
	err := viper.ReadInConfig()

	if err != nil {
		log.Fatal(err)
	} else {
		bucket = viper.GetString("s3.bucket")
	}

	router := httprouter.New()
	router.GET("/:size/*source", handleResize)
	log.Fatal(http.ListenAndServe(":"+strconv.Itoa(viper.GetInt("server.port")), router))
}

func handleResize(writer http.ResponseWriter, request *http.Request, params httprouter.Params) {
	sourcePath := request.URL.EscapedPath()
	width, height, err := parseWidthAndHeight(params.ByName("size"))

	if err != nil {
		http.Error(writer, err.Error(), 601)
		return
	}

	signature := request.Header.Get("Signature")

	if err = validateSignature(signature, sourcePath); err != nil {
		http.Error(writer, err.Error(), 602)
		return
	}

	source, err := url.Parse(strings.TrimPrefix(params.ByName("source"), "/"))

	if err != nil {
		http.Error(writer, err.Error(), 603)
		return
	}

	source.Scheme = ""
	source.Host = ""
	dir, file := path.Split(source.String())
	resultPath := strings.Join([]string{"cache/", dir, params.ByName("size"), "/", file}, "")

	if bucket == "" {
		body, e := getImageFromURL(source.String())

		if e != nil {
			http.Error(writer, e.Error(), 604)
			return
		}

		e = generateThumbnail(writer, body, sourcePath, width, height)

		if e != nil {
			http.Error(writer, e.Error(), 605)
			return
		}

		return
	}

	config := &aws.Config{
		Region: aws.String(viper.GetString("s3.region")),
		Credentials: credentials.NewStaticCredentials(
			viper.GetString("s3.access-key-id"),
			viper.GetString("s3.secret-access-key"),
			"",
		),
	}

	sess, err := session.NewSession(config)

	if err != nil {
		http.Error(writer, err.Error(), 606)
		return
	}

	input := &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(resultPath),
	}

	svc := s3.New(sess)
	output, err := svc.GetObject(input)

	if err != nil {
		source, err := url.Parse(strings.TrimPrefix(params.ByName("source"), "/"))

		if err != nil {
			http.Error(writer, err.Error(), 607)
			return
		}

		if source.Host == "" {
			input := &s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(params.ByName("source")),
			}

			output, err = svc.GetObject(input)

			if err != nil {
				http.Error(writer, err.Error(), 608)
				return
			}

			err = generateThumbnail(writer, output.Body, resultPath, width, height)

			if err != nil {
				http.Error(writer, err.Error(), 609)
				return
			}
		} else {
			body, err := getImageFromURL(source.String())

			if err != nil {
				http.Error(writer, err.Error(), 610)
			}

			generateThumbnail(writer, body, resultPath, width, height)
			return
		}
	}

	setResultHeaders(writer, &result{
		ContentType:   *output.ContentType,
		ContentLength: *output.ContentLength,
		ETag:          *output.ETag,
		Path:          resultPath,
	})

	if _, err := io.Copy(writer, output.Body); err != nil {
		http.Error(writer, err.Error(), 611)
		return
	}
}

type result struct {
	Data          []byte
	ContentType   string
	ContentLength int64
	ETag          string
	Path          string
}

func computeHexMD5(data []byte) string {
	h := md5.New()
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func generateThumbnail(writer http.ResponseWriter, body io.ReadCloser, path string, width, height int) error {
	img, err := ioutil.ReadAll(body)
	body.Close()

	if err != nil {
		return err
	}

	buf, err := vips.Resize(img, vips.Options{
		Height:       height,
		Width:        width,
		Crop:         viper.GetBool("vips.crop"),
		Interpolator: vips.BICUBIC,
		Gravity:      vips.CENTRE,
		Quality:      viper.GetInt("vips.quality"),
	})

	if err != nil {
		return err
	}

	var contentType string

	switch {
	case bytes.Equal(buf[:2], vips.MARKER_JPEG):
		contentType = "image/jpeg"
	case bytes.Equal(buf[:2], vips.MARKER_PNG):
		contentType = "image/png"
	default:
		return fmt.Errorf("Unknown image format")
	}

	result := &result{
		ContentType:   contentType,
		ContentLength: int64(len(buf)),
		Data:          buf,
		ETag:          computeHexMD5(buf),
		Path:          path,
	}

	setResultHeaders(writer, result)

	if _, err = writer.Write(buf); err != nil {
		return err
	}

	if bucket != "" {
		go storeResult(result)
	}

	return nil
}

func getImageFromURL(URL string) (io.ReadCloser, error) {
	response, err := httpClient.Get(URL)

	if err != nil {
		return nil, err
	}

	if response.StatusCode != 200 {
		return nil, fmt.Errorf("Unexpected status code from source: %d", response.StatusCode)
	}

	return response.Body, nil
}

func parseWidthAndHeight(str string) (width, height int, err error) {
	if value, ok := viper.GetStringMapString("sizes")[str]; ok {
		sizeParts := strings.Split(value, "x")

		if len(sizeParts) != 2 {
			return 0, 0, fmt.Errorf("Invalid size requested")
		}

		width, err = strconv.Atoi(sizeParts[0])

		if err != nil {
			return 0, 0, err
		}

		height, err = strconv.Atoi(sizeParts[1])

		if err != nil {
			return 0, 0, err
		}

		return width, height, nil
	}

	err = fmt.Errorf("Invalid size requested")
	return
}

func setCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d,public", viper.GetInt("cache-control.max-age")))
}

func setResultHeaders(w http.ResponseWriter, result *result) {
	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(result.ContentLength, 10))
	w.Header().Set("ETag", `"`+result.ETag+`"`)
	setCacheHeaders(w)
}

func storeResult(result *result) {
	config := &aws.Config{
		Region: aws.String(viper.GetString("s3.region")),
		Credentials: credentials.NewStaticCredentials(
			viper.GetString("s3.access-key-id"),
			viper.GetString("s3.secret-access-key"),
			"",
		),
	}

	session, err := session.NewSession(config)

	if err != nil {
		log.Fatal(err)
	}

	svc := s3.New(session)

	params := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(result.Path),
		Body:          bytes.NewReader(result.Data),
		ContentLength: aws.Int64(result.ContentLength),
		ContentType:   aws.String(result.ContentType),
		StorageClass:  aws.String(s3.StorageClassReducedRedundancy),
	}

	_, err = svc.PutObject(params)

	if err != nil {
		log.Fatal(err)
	}
}

func validateSignature(sig, pathPart string) error {
	h := hmac.New(sha3.New256, []byte(viper.GetString("server.key")))

	if _, err := h.Write([]byte(pathPart)); err != nil {
		return err
	}

	actualSig := base64.StdEncoding.EncodeToString(h.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(sig), []byte(actualSig)) != 1 {
		return fmt.Errorf("Signature mismatch")
	}

	return nil
}
