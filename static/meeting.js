// Meeting page: values come from data-* attributes (no inline script, no
// template interpolation inside JavaScript).
(function () {
	var el = document.getElementById("meet")
	var d = el.dataset
	var api = new JitsiMeetExternalAPI("8x8.vc", {
		roomName: d.appId + "/" + d.room,
		jwt: d.jwt,
		parentNode: el,
		userInfo: { displayName: d.name },
	})
	el.removeAttribute("data-jwt")

	function showEnded() {
		var msg = document.createElement("div")
		msg.className = "ended"
		msg.textContent = "La reunión terminó."
		document.body.replaceChildren(msg)
	}

	var stopCapture = function () {}

	if (d.moderator === "true") {
		var endURL = "/m/" + encodeURIComponent(d.room) + "/end"
		var body = new URLSearchParams({ csrf_token: d.csrf })
		var end = function () {
			navigator.sendBeacon(endURL, body)
		}
		api.addEventListener("readyToClose", function () {
			stopCapture()
			end()
			showEnded()
		})
		window.addEventListener("pagehide", end)
	} else {
		api.addEventListener("readyToClose", function () {
			stopCapture()
			showEnded()
		})
	}

	if (d.capture === "true") {
		api.addEventListener("videoConferenceJoined", function () {
			stopCapture = startCapture()
		})
	}

	// startCapture records this participant's own microphone for the meeting
	// transcript. While the participant is muted in the meeting the track is
	// disabled, so only silence is recorded. Audio is uploaded every 15 s.
	function startCapture() {
		var badge = document.createElement("div")
		badge.className = "capture-badge"
		badge.setAttribute("role", "status")
		document.body.appendChild(badge)
		var say = function (text, cls) {
			badge.textContent = text
			badge.className = "capture-badge" + (cls ? " " + cls : "")
		}
		var mime = "audio/webm;codecs=opus"
		if (!window.MediaRecorder || !navigator.mediaDevices || !MediaRecorder.isTypeSupported(mime)) {
			say("Este navegador no permite transcribir tu micrófono.", "off")
			return function () {}
		}
		var stopped = false
		var cleanup = function () {
			stopped = true
		}
		say("Pidiendo permiso del micrófono para la transcripción…")
		navigator.mediaDevices
			.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true, channelCount: 1 } })
			.then(function (stream) {
				if (stopped) {
					stream.getTracks().forEach(function (t) { t.stop() })
					return
				}
				var track = stream.getAudioTracks()[0]
				var setMuted = function (muted) {
					track.enabled = !muted
					say(muted ? "Micrófono silenciado: no se transcribe." : "● Transcribiendo tu micrófono", muted ? "muted" : "")
				}
				api.isAudioMuted().then(setMuted)
				api.addEventListener("audioMuteStatusChanged", function (e) {
					setMuted(e.muted)
				})

				var id = new Uint8Array(18)
				crypto.getRandomValues(id)
				var streamId = btoa(String.fromCharCode.apply(null, id)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "")
				var start = Date.now()
				var url = "/m/" + encodeURIComponent(d.room) + "/audio?stream=" + streamId + "&start=" + start + "&seq="
				var seq = 0
				var queue = Promise.resolve()
				var failed = false
				var wait = function (ms) {
					return new Promise(function (r) { setTimeout(r, ms) })
				}
				var upload = function (n, blob, attempt) {
					return fetch(url + n, {
						method: "POST",
						headers: { "Content-Type": "audio/webm", "X-CSRF-Token": d.csrf },
						body: blob,
						credentials: "same-origin",
						keepalive: blob.size < 60000,
					})
						.then(function (r) {
							if (r.ok) return
							if (r.status >= 500 && attempt < 3) return wait(2000 * (attempt + 1)).then(function () { return upload(n, blob, attempt + 1) })
							throw new Error("upload " + r.status)
						})
						.catch(function (err) {
							if (attempt < 3 && !(err && /upload/.test(err.message))) {
								return wait(2000 * (attempt + 1)).then(function () { return upload(n, blob, attempt + 1) })
							}
							failed = true
							say("La transcripción de tu micrófono se interrumpió.", "off")
						})
				}
				var rec = new MediaRecorder(stream, { mimeType: mime, audioBitsPerSecond: 16000 })
				rec.ondataavailable = function (ev) {
					if (failed || !ev.data || !ev.data.size) return
					var n = seq++
					queue = queue.then(function () { return upload(n, ev.data, 0) })
				}
				rec.start(15000)
				cleanup = function () {
					stopped = true
					if (rec.state !== "inactive") rec.stop()
					track.stop()
					badge.remove()
				}
				window.addEventListener("pagehide", cleanup)
			})
			.catch(function () {
				say("Transcripción desactivada: no se dio permiso al micrófono.", "off")
			})
		return function () {
			cleanup()
		}
	}
})()
