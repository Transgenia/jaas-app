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

	if (d.moderator === "true") {
		var endURL = "/m/" + encodeURIComponent(d.room) + "/end"
		var body = new URLSearchParams({ csrf_token: d.csrf })
		var end = function () {
			navigator.sendBeacon(endURL, body)
		}
		api.addEventListener("readyToClose", function () {
			end()
			showEnded()
		})
		window.addEventListener("pagehide", end)
	} else {
		api.addEventListener("readyToClose", showEnded)
	}
})()
