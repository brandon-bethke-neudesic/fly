package integration_test

import (
	"archive/tar"
	"compress/gzip"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/onsi/gomega/ghttp"

	"github.com/concourse/atc/api/resources"
	tbuilds "github.com/concourse/turbine/api/builds"
	"github.com/concourse/turbine/event"
)

var _ = Describe("Fly CLI", func() {
	var flyPath string
	var buildDir string

	var atcServer *ghttp.Server
	var streaming chan *websocket.Conn
	var uploadingBits <-chan struct{}

	var expectedTurbineBuild tbuilds.Build

	BeforeEach(func() {
		var err error

		flyPath, err = gexec.Build("github.com/concourse/fly")
		Ω(err).ShouldNot(HaveOccurred())

		buildDir, err = ioutil.TempDir("", "fly-build-dir")
		Ω(err).ShouldNot(HaveOccurred())

		err = ioutil.WriteFile(
			filepath.Join(buildDir, "build.yml"),
			[]byte(`---
image: ubuntu

params:
  FOO: bar
  BAZ: buzz
  X: 1

run:
  path: find
  args: [.]
`),
			0644,
		)
		Ω(err).ShouldNot(HaveOccurred())

		atcServer = ghttp.NewServer()

		os.Setenv("ATC_URL", atcServer.URL())

		streaming = make(chan *websocket.Conn, 1)

		expectedTurbineBuild = tbuilds.Build{
			Privileged: true,
			Config: tbuilds.Config{
				Image: "ubuntu",
				Params: map[string]string{
					"FOO": "bar",
					"BAZ": "buzz",
					"X":   "1",
				},
				Run: tbuilds.RunConfig{
					Path: "find",
					Args: []string{"."},
				},
			},

			Inputs: []tbuilds.Input{
				{
					Name: filepath.Base(buildDir),
					Type: "archive",
					Source: tbuilds.Source{
						"uri": "http://127.0.0.1:1234/api/v1/pipes/some-pipe-id",
					},
				},
			},
		}
	})

	JustBeforeEach(func() {
		uploading := make(chan struct{})
		uploadingBits = uploading

		atcServer.AppendHandlers(
			ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", "/api/v1/pipes"),
				ghttp.RespondWithJSONEncoded(http.StatusCreated, resources.Pipe{
					ID:       "some-pipe-id",
					PeerAddr: "127.0.0.1:1234",
				}),
			),
			ghttp.CombineHandlers(
				ghttp.VerifyRequest("POST", "/api/v1/builds"),
				ghttp.VerifyJSONRepresenting(expectedTurbineBuild),
				func(w http.ResponseWriter, r *http.Request) {
					http.SetCookie(w, &http.Cookie{
						Name:    "Some-Cookie",
						Value:   "some-cookie-data",
						Path:    "/",
						Expires: time.Now().Add(1 * time.Minute),
					})
				},
				ghttp.RespondWith(201, `{"id":128}`),
			),
			ghttp.CombineHandlers(
				ghttp.VerifyRequest("GET", "/api/v1/builds/128/events"),
				func(w http.ResponseWriter, r *http.Request) {
					upgrader := websocket.Upgrader{
						CheckOrigin: func(r *http.Request) bool {
							// allow all connections
							return true
						},
					}

					cookie, err := r.Cookie("Some-Cookie")
					Ω(err).ShouldNot(HaveOccurred())
					Ω(cookie.Value).Should(Equal("some-cookie-data"))

					conn, err := upgrader.Upgrade(w, r, nil)
					Ω(err).ShouldNot(HaveOccurred())

					err = conn.WriteJSON(event.VersionMessage{Version: "1.0"})
					Ω(err).ShouldNot(HaveOccurred())

					streaming <- conn
				},
			),
			ghttp.CombineHandlers(
				ghttp.VerifyRequest("PUT", "/api/v1/pipes/some-pipe-id"),
				func(w http.ResponseWriter, req *http.Request) {
					close(uploading)

					gr, err := gzip.NewReader(req.Body)
					Ω(err).ShouldNot(HaveOccurred())

					tr := tar.NewReader(gr)

					hdr, err := tr.Next()
					Ω(err).ShouldNot(HaveOccurred())

					Ω(hdr.Name).Should(Equal("./"))

					hdr, err = tr.Next()
					Ω(err).ShouldNot(HaveOccurred())

					Ω(hdr.Name).Should(MatchRegexp("(./)?build.yml$"))
				},
				ghttp.RespondWith(200, ""),
			),
		)
	})

	It("creates a build, streams output, uploads the bits, and polls until completion", func() {
		flyCmd := exec.Command(flyPath)
		flyCmd.Dir = buildDir

		sess, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
		Ω(err).ShouldNot(HaveOccurred())

		var stream *websocket.Conn
		Eventually(streaming).Should(Receive(&stream))

		err = stream.WriteJSON(event.Message{
			event.Log{Payload: "sup"},
		})
		Ω(err).ShouldNot(HaveOccurred())

		Eventually(sess.Out).Should(gbytes.Say("sup"))
	})

	Context("when arguments are passed through", func() {
		BeforeEach(func() {
			expectedTurbineBuild.Config.Run.Args = []string{".", "-name", `foo "bar" baz`}
		})

		It("inserts them into the config template", func() {
			atcServer.AllowUnhandledRequests = true

			flyCmd := exec.Command(flyPath, "--", "-name", "foo \"bar\" baz")
			flyCmd.Dir = buildDir

			_, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Ω(err).ShouldNot(HaveOccurred())

			// sync with after create
			Eventually(streaming, 5.0).Should(Receive())
		})
	})

	Context("when paramters are specified in the environment", func() {
		BeforeEach(func() {
			expectedTurbineBuild.Config.Params = map[string]string{
				"FOO": "newbar",
				"BAZ": "buzz",
				"X":   "",
			}
		})

		It("overrides the build's paramter values", func() {
			atcServer.AllowUnhandledRequests = true

			flyCmd := exec.Command(flyPath)
			flyCmd.Dir = buildDir
			flyCmd.Env = append(os.Environ(), "FOO=newbar", "X=")

			_, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Ω(err).ShouldNot(HaveOccurred())

			// sync with after create
			Eventually(streaming, 5.0).Should(Receive())
		})
	})

	Context("when the build is interrupted", func() {
		var aborted chan struct{}

		JustBeforeEach(func() {
			aborted = make(chan struct{})

			atcServer.AppendHandlers(
				ghttp.CombineHandlers(
					ghttp.VerifyRequest("POST", "/api/v1/builds/128/abort"),
					func(w http.ResponseWriter, r *http.Request) {
						close(aborted)
					},
				),
			)
		})

		Describe("with SIGINT", func() {
			It("aborts the build and exits nonzero", func() {
				flyCmd := exec.Command(flyPath)
				flyCmd.Dir = buildDir

				flySession, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ToNot(HaveOccurred())

				var stream *websocket.Conn
				Eventually(streaming, 5).Should(Receive(&stream))

				Eventually(uploadingBits).Should(BeClosed())

				flySession.Signal(syscall.SIGINT)

				Eventually(aborted, 5.0).Should(BeClosed())

				err = stream.WriteJSON(event.Message{
					event.Status{Status: tbuilds.StatusErrored},
				})
				Ω(err).ShouldNot(HaveOccurred())

				Eventually(flySession, 5.0).Should(gexec.Exit(2))
			})
		})

		Describe("with SIGTERM", func() {
			It("aborts the build and exits nonzero", func() {
				flyCmd := exec.Command(flyPath)
				flyCmd.Dir = buildDir

				flySession, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
				Expect(err).ToNot(HaveOccurred())

				var stream *websocket.Conn
				Eventually(streaming, 5).Should(Receive(&stream))

				Eventually(uploadingBits).Should(BeClosed())

				flySession.Signal(syscall.SIGTERM)

				Eventually(aborted, 5.0).Should(BeClosed())

				err = stream.WriteJSON(event.Message{
					event.Status{Status: tbuilds.StatusErrored},
				})
				Ω(err).ShouldNot(HaveOccurred())

				Eventually(flySession, 5.0).Should(gexec.Exit(2))
			})
		})
	})

	Context("when the build succeeds", func() {
		It("exits 0", func() {
			flyCmd := exec.Command(flyPath)
			flyCmd.Dir = buildDir

			flySession, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			var stream *websocket.Conn
			Eventually(streaming, 5).Should(Receive(&stream))

			err = stream.WriteJSON(event.Message{
				event.Status{Status: tbuilds.StatusSucceeded},
			})
			Ω(err).ShouldNot(HaveOccurred())

			Eventually(flySession, 5.0).Should(gexec.Exit(0))
		})
	})

	Context("when the build fails", func() {
		It("exits 1", func() {
			flyCmd := exec.Command(flyPath)
			flyCmd.Dir = buildDir

			flySession, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			var stream *websocket.Conn
			Eventually(streaming, 5).Should(Receive(&stream))

			err = stream.WriteJSON(event.Message{
				event.Status{Status: tbuilds.StatusFailed},
			})
			Ω(err).ShouldNot(HaveOccurred())

			Eventually(flySession, 5.0).Should(gexec.Exit(1))
		})
	})

	Context("when the build errors", func() {
		It("exits 2", func() {
			flyCmd := exec.Command(flyPath)
			flyCmd.Dir = buildDir

			flySession, err := gexec.Start(flyCmd, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			var stream *websocket.Conn
			Eventually(streaming, 5).Should(Receive(&stream))

			err = stream.WriteJSON(event.Message{
				event.Status{Status: tbuilds.StatusErrored},
			})
			Ω(err).ShouldNot(HaveOccurred())

			Eventually(flySession, 5.0).Should(gexec.Exit(2))
		})
	})
})
