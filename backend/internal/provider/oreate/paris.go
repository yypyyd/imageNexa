package oreate

import (
	"net/url"
	"strings"
	"time"
)

const parisReadyTimeout = 55 * time.Second

// parisReadyJS is true once the official page has created a Banti instance.
// Oreate no longer pins a single window.paris_<sak> name before the 3MB
// errorMonitor bundle finishes; the live SDK stores instances in
// PARIS_INSTANCE_CACHE and also assigns window["paris_"+sid/sak].
const parisReadyJS = `(function(){
  var cache = window.PARIS_INSTANCE_CACHE;
  if (cache) {
    for (var key in cache) {
      if (!Object.prototype.hasOwnProperty.call(cache, key)) continue;
      if (cache[key] && typeof cache[key].sendBantiReport === "function") return true;
    }
  }
  var names = Object.getOwnPropertyNames(window);
  for (var i = 0; i < names.length; i++) {
    if (!/^paris_/i.test(names[i])) continue;
    var inst = window[names[i]];
    if (inst && typeof inst.sendBantiReport === "function") return true;
  }
  return false;
})()`

const parisHelperJS = `window.oreateParisInstance = function() {
  var cache = window.PARIS_INSTANCE_CACHE;
  if (cache) {
    for (var key in cache) {
      if (!Object.prototype.hasOwnProperty.call(cache, key)) continue;
      var cached = cache[key];
      if (cached && typeof cached.sendBantiReport === "function") return cached;
    }
  }
  var names = Object.getOwnPropertyNames(window);
  for (var i = 0; i < names.length; i++) {
    if (!/^paris_/i.test(names[i])) continue;
    var inst = window[names[i]];
    if (inst && typeof inst.sendBantiReport === "function") return inst;
  }
  return null;
};
window.oreateSendBantiReport = function(payload, callback) {
  var paris = window.oreateParisInstance();
  if (!paris) {
    callback(new Error("banti instance"));
    return;
  }
  if (paris.options) paris.options.reportTimeout = 20000;
  if (typeof paris.sendBantiReport === "function") {
    paris.sendBantiReport(payload, callback);
    return;
  }
  if (typeof paris.getBantiInstance !== "function") {
    callback(new Error("banti instance"));
    return;
  }
  paris.getBantiInstance(function(instanceError, instance) {
    if (instanceError || !instance) {
      callback(instanceError || new Error("banti instance"));
      return;
    }
    if (instance.options) instance.options.reportTimeout = 20000;
    instance.sendBantiReport(payload, callback);
  });
};
window.oreateGetAcsToken = function() {
  return new Promise(function(resolve) {
    var timer = setTimeout(function() { resolve("600"); }, 8000);
    var paris = window.oreateParisInstance();
    if (!paris || typeof paris.getAcsToken !== "function") {
      clearTimeout(timer);
      resolve("600");
      return;
    }
    try {
      paris.getAcsToken(function(token) {
        clearTimeout(timer);
        resolve(typeof token === "string" && token ? token : "600");
      });
    } catch (error) {
      clearTimeout(timer);
      resolve("600");
    }
  });
};
`

func mintBantiEvaluateJS() string {
	return parisHelperJS + `
		window.__oreateSignerJT = "";
		window.oreateSendBantiReport({subid: ""}, function(reportError, response, fallback) {
			if (reportError) return;
			window.__oreateSignerJT = (response && response.htj && response.htj.jt) || fallback || "";
		});
		true`
}

func mintBantiDispatchJS() string {
	return parisHelperJS + `
		window.__oreateSignerJT = "";
		window.__oreateSignerDiag = {startedAt: performance.now()};
		(function(){
			var paris = window.oreateParisInstance();
			var diag = window.__oreateSignerDiag;
			diag.instancePresent = Boolean(paris);
			diag.globalSend = Boolean(paris && typeof paris.sendBantiReport === "function");
			diag.optionsPresent = Boolean(paris && paris.options);
			diag.reportTimeoutBefore = Number(paris && paris.options && paris.options.reportTimeout) || 0;
			diag.instanceAt = performance.now();
			window.oreateSendBantiReport({subid: ""}, function(reportError, response, fallback) {
				diag.sendAt = diag.sendAt || performance.now();
				diag.callbackAt = performance.now();
				diag.callbackError = Boolean(reportError);
				diag.responsePresent = Boolean(response);
				diag.htjPresent = Boolean(response && response.htj);
				var jt = (response && response.htj && response.htj.jt) || fallback || "";
				diag.jtLength = typeof jt === "string" ? jt.length : 0;
				window.__oreateSignerJT = jt;
			});
			diag.sendAt = performance.now();
			diag.reportTimeoutAfter = Number(paris && paris.options && paris.options.reportTimeout) || 0;
		})();
		true`
}

func isBantiReportURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "banti.oreateai.com" && !(strings.Contains(host, "banti") && strings.HasSuffix(host, ".oreateai.com")) {
		return false
	}
	path := parsed.EscapedPath()
	return path == "/dr" || strings.Contains(path, "/dr")
}

func clipSignerLog(err error) string {
	if err == nil {
		return ""
	}
	value := strings.Join(strings.Fields(err.Error()), " ")
	if len(value) > 800 {
		return value[:800]
	}
	return value
}
