package managementasset

import "bytes"

// managementUAStabilizerScript is a transitional shim for management panel builds
// that derive their localStorage obfuscation key from the full User-Agent. It
// re-keys stored values onto a version-free User-Agent and pins
// navigator.userAgent to it, so a browser update no longer loses saved logins.
// Panel builds that fix the key themselves carry the cpa_obfuscation_ua_migrated
// marker and are served unchanged.
const managementUAStabilizerScript = `<script>(function(){try{var S="cli-proxy-api-webui::secure-storage",P="enc::v1::",M="cpa_obfuscation_ua_migrated";var rawUA=navigator.userAgent||"";var canonUA=rawUA.replace(/\b(Chrome|CriOS|Chromium|Edg|EdgA|EdgiOS|Firefox|FxiOS|Version|OPR|Brave|Vivaldi)\/\d+(?:\.\d+)*/g,"$1/0");function keyFor(ua){return new TextEncoder().encode(S+"|"+location.host+"|"+ua)}function xor(d,k){var r=new Uint8Array(d.length);for(var i=0;i<d.length;i++)r[i]=d[i]^k[i%k.length];return r}function dec(v,k){var b=atob(v.slice(P.length)),a=new Uint8Array(b.length);for(var i=0;i<b.length;i++)a[i]=b.charCodeAt(i);return new TextDecoder().decode(xor(a,k))}function enc(v,k){var x=xor(new TextEncoder().encode(v),k),s="";for(var i=0;i<x.length;i++)s+=String.fromCharCode(x[i]);return P+btoa(s)}if(rawUA&&rawUA!==canonUA&&typeof localStorage!=="undefined"&&localStorage.getItem(M)!=="v1"){var oldK=keyFor(rawUA),newK=keyFor(canonUA);for(var i=0;i<localStorage.length;i++){var k=localStorage.key(i);if(!k)continue;var val=localStorage.getItem(k);if(!val||val.indexOf(P)===-1)continue;var updated=val.replace(/enc::v1::[A-Za-z0-9+/=]+/g,function(tok){try{return enc(dec(tok,oldK),newK)}catch(_){return tok}});if(updated!==val)localStorage.setItem(k,updated)}localStorage.setItem(M,"v1")}Object.defineProperty(navigator,"userAgent",{get:function(){return canonUA},configurable:true})}catch(_){}})();</script>`

// PatchManagementHTML injects a browser-version-invariant User-Agent shim into
// management.html when it uses the upstream navigator.userAgent XOR obfuscation key,
// preventing browser upgrades (e.g. Chrome 154 -> 155) from corrupting saved localStorage values.
func PatchManagementHTML(data []byte) []byte {
	if !bytes.Contains(data, []byte("cli-proxy-api-webui::secure-storage")) ||
		bytes.Contains(data, []byte("cpa_obfuscation_ua_migrated")) {
		return data
	}
	scriptBytes := []byte(managementUAStabilizerScript)
	if idx := bytes.Index(bytes.ToLower(data), []byte("<head>")); idx >= 0 {
		insertPos := idx + len("<head>")
		out := make([]byte, 0, len(data)+len(scriptBytes))
		out = append(out, data[:insertPos]...)
		out = append(out, scriptBytes...)
		out = append(out, data[insertPos:]...)
		return out
	}
	out := make([]byte, 0, len(scriptBytes)+len(data))
	out = append(out, scriptBytes...)
	out = append(out, data...)
	return out
}
