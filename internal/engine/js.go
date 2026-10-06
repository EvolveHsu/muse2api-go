// 本文件由 tools/gen_js.mjs 从真 Python 的 ast 抽取结果生成，请勿手改。
// 金样本：internal/engine/testdata/engine_js.json
// 校验：internal/engine/js_test.go 会逐条比对这里每个常量与拼接模板。
package engine

const (
	// engine.py 模块级常量 ATT_SEL
	attSel = `[data-testid^="hatch-chat-attachment-presentation-"]`

	// engine.py:415
	clickJS = `(function(){var sel=%s;var b=[...document.querySelectorAll(sel)].filter(function(x){return x.offsetParent!==null;})[0];if(!b)return null;var r=b.getBoundingClientRect();return JSON.stringify({x:Math.round(r.x+r.width/2),y:Math.round(r.y+r.height/2)});})()`

	// engine.py:504
	attJS = `(function(){var list = []; var seen = new Set();function addEl(el, tid){  if(!el || seen.has(el)) return;  seen.add(el);  if(el.closest('form, [class*=chat-user-bubble], [class*="group/msg"]')) return;  var v = el.querySelector('video') || (el.tagName === 'VIDEO' ? el : null);  var img = el.querySelector('img') || (el.tagName === 'IMG' ? el : null);  var isVid = (tid || '').includes('video') || !!v;  var primary = isVid ? (v || img) : (img || v);  var src = primary ? (primary.currentSrc || primary.src || '') : '';  if(src && !seen.has(src)){    seen.add(src);    list.push({      tid: tid || el.getAttribute('data-testid') || (isVid ? 'video' : 'image'),      hasVideo: !!v,      hasImg: !!img,      src: src,      vSrc: v ? (v.currentSrc || v.src || '') : '',      iSrc: img ? (img.currentSrc || img.src || '') : '',      w: primary ? (primary.videoWidth || primary.naturalWidth || 0) : 0,      h: primary ? (primary.videoHeight || primary.naturalHeight || 0) : 0    });  }}document.querySelectorAll('[data-testid^="hatch-chat-attachment-presentation-"]').forEach(function(a){ addEl(a, a.getAttribute('data-testid')); });document.querySelectorAll('div[class*="hatch-agent-bubble-bg"] img, div[class*="hatch-agent-bubble-bg"] video').forEach(function(m){  var s = m.currentSrc || m.src || '';  if(s && !s.includes('avatar') && !s.includes('emoji')) addEl(m.parentElement || m, 'agent-media');});return JSON.stringify(list);})()`

	// engine.py:550
	sendStateJS = `(function(){var ta=document.querySelector('textarea');var b=[...document.querySelectorAll('button,[role=button]')].find(function(x){return /send/i.test(x.getAttribute('aria-label')||'');});return JSON.stringify({v:ta?ta.value:'',btn:b?(b.disabled?2:1):0});})()`

	// engine.py:774
	extractJS = `
    (async function(src, expect){
      try{
        var u = src;
        if(!u) return JSON.stringify({ok:false,err:'no-media-src'});
        var r = await fetch(u);
        if(!r.ok) return JSON.stringify({ok:false,err:'media-http-'+r.status});
        var b = await r.blob();
        var ab = await b.arrayBuffer();
        var bytes = new Uint8Array(ab);
        var s = '';
        for(var i=0; i<bytes.length; i+=65536){
          s += String.fromCharCode.apply(null, bytes.subarray(i, i+65536));
        }
        return JSON.stringify({ok:true, mime:b.type||'', size:b.size, url:u, b64:btoa(s)});
      }catch(e){
        return JSON.stringify({ok:false, err:String(e)});
      }
    })(%s, %s)
    `

	// engine.py:797
	agentTextJS = `(function(){var bs=[].slice.call(document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]'));for(var i=bs.length-1;i>=0;i--){var cs=bs[i].className||'';if(/hatch-agent-bubble-bg/.test(cs))return bs[i].innerText||'';}return '';})()`

	// engine.py:806
	userCountJS = `(function(){var bs=[].slice.call(document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]'));var n=0;for(var i=0;i<bs.length;i++){if(/chat-user-bubble/.test(bs[i].className||''))n++;}return String(n);})()`

	// engine.py:814
	agentCountJS = `(function(){var bs=[].slice.call(document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]'));var n=0;for(var i=0;i<bs.length;i++){if(/hatch-agent-bubble-bg/.test(bs[i].className||''))n++;}return String(n);})()`

	// engine.py:822
	pollChatJS = `(function(){var els=[...document.querySelectorAll('*')].filter(function(e){var s=getComputedStyle(e);return (s.overflowY==='auto'||s.overflowY==='scroll')&&e.scrollHeight>e.clientHeight+100;});els.sort(function(a,b){return b.scrollHeight-a.scrollHeight;});if(els[0])els[0].scrollTop=els[0].scrollHeight;var scope=document.querySelector('main,[class*="chat-scroll"],[class*="hatch-chat-scroll"]')||document.body;var bs=[].slice.call(scope.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]')).filter(function(b){return /hatch-agent-bubble-bg/.test(b.className||'');});var nonEmpty=bs.filter(function(b){return ((b.innerText||'').trim().length)>0;});var txt=nonEmpty.length?(nonEmpty[nonEmpty.length-1].innerText||'').trim():'';var stop=!!(document.querySelector('[data-testid="hatch-composer-stop-button"]')||document.querySelector('button[aria-label*="Stop" i]')||document.querySelector('button[aria-label*="停止"]'));return JSON.stringify({cnt:nonEmpty.length,total:bs.length,txt:txt,stop:stop});})()`

	// engine.py:587
	setterJS = `(function(t){var ta=document.querySelector('textarea');if(!ta) return 0;ta.focus();var s=Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set;s.call(ta,t);ta.dispatchEvent(new Event('input',{bubbles:true}));ta.dispatchEvent(new Event('change',{bubbles:true}));return (ta.value||'').length;})(%s)`

	// engine.py:1050
	injectJS = `
        (function(b64, mime) {
            try {
                var byteChars = atob(b64);
                var byteNumbers = new Array(byteChars.length);
                for (var i = 0; i < byteChars.length; i++) {
                    byteNumbers[i] = byteChars.charCodeAt(i);
                }
                var byteArray = new Uint8Array(byteNumbers);
                var blob = new Blob([byteArray], {type: mime});
                var ext = mime.split('/')[1] || 'png';
                if (ext === 'jpeg') ext = 'jpg';
                var file = new File([blob], 'reference_image.' + ext, {type: mime});
                // 现网 composer 里可能有多个 file input；优先取位于 composer 内的那个
                var inputs = Array.from(document.querySelectorAll('input[type="file"]'));
                if (!inputs.length) return JSON.stringify({ok: false, err: 'no-file-input'});
                var ov = document.querySelector('[data-testid="hatch-composer-placeholder-overlay"]');
                var composer = ov ? ov.closest('form') : null;
                var input = null;
                if (composer) {
                    input = inputs.find(function(x){ return composer.contains(x); }) || null;
                }
                if (!input) {
                    // 退而求其次：取不在对话气泡里的那个 input
                    input = inputs.find(function(x){
                        return !x.closest('[class*=chat-user-bubble], [class*="group/msg"]');
                    }) || inputs[0];
                }
                // 有些版本 accept 为空，补上 image/* 让页面接受本次 File
                if (!input.getAttribute('accept')) input.setAttribute('accept', 'image/*');
                var dt = new DataTransfer();
                dt.items.add(file);
                input.files = dt.files;
                input.dispatchEvent(new Event('change', {bubbles: true}));
                input.dispatchEvent(new Event('input', {bubbles: true}));
                return JSON.stringify({ok: true});
            } catch(e) {
                return JSON.stringify({ok: false, err: String(e)});
            }
        })(%s, %s)
        `

	// engine.py:438
	jsQuotaL438 = `(function(){var els=[...document.querySelectorAll('div,span,li,[role=menuitem],button')].filter(function(e){return e.offsetParent!==null&&(e.textContent||'').trim()==='Settings'&&e.getAttribute('aria-label')!=='Settings'&&e.children.length<=3;});if(!els.length)return null;var el=els[els.length-1];var r=el.getBoundingClientRect();return JSON.stringify({x:Math.round(r.x+r.width/2),y:Math.round(r.y+r.height/2)});})()`

	// engine.py:571
	jsSendL571 = `(function(){var t=document.querySelector('textarea');if(!t)return null;var r=t.getBoundingClientRect();return JSON.stringify({x:Math.round(r.left+r.width/2),y:Math.round(r.top+r.height/2)});})()`

	// engine.py:377
	jsEnsurePageL377 = `!!document.querySelector('textarea')`

	// engine.py:559
	jsSendL559 = `(function(){
                var ta = document.querySelector('textarea');
                if (ta) {
                    ta.scrollIntoView({block: 'center', inline: 'nearest'});
                    ta.focus();
                }
            })()`

	// engine.py:605
	jsSendL605 = `(function(){var b=[...document.querySelectorAll('button,[role=button]')].filter(function(x){return x.offsetParent!==null;}).find(function(x){return /发送|send/i.test(x.getAttribute('aria-label')||'')||/发送|send/i.test(x.getAttribute('data-testid')||'')||/send/i.test(x.innerText||'');});if(!b)return 'no-button';if(b.disabled)return 'disabled';b.click();return 'clicked';})()`

	// engine.py:649
	jsScrollBottomL649 = `(function(){var els=[...document.querySelectorAll('*')].filter(function(e){var s=getComputedStyle(e);return (s.overflowY==='auto'||s.overflowY==='scroll')&&e.scrollHeight>e.clientHeight+100;});els.sort(function(a,b){return b.scrollHeight-a.scrollHeight;});if(els[0])els[0].scrollTop=els[0].scrollHeight;var s=document.scrollingElement||document.body;s.scrollTop=s.scrollHeight;var el=document.querySelector('textarea');if(el)el.scrollIntoView({block:'end'});return 1;})()`

	// engine.py:965
	jsDownloadFallbackL965 = `(function(src){
            var media=[...document.querySelectorAll('img,video')]
                .find(m=>(m.currentSrc||m.src||'')===src);
            var node=media && media.closest('[data-testid^="hatch-chat-attachment-presentation-"]');
            if(!node) return 'none';
            node=node.closest('[class*="group/widget-presentation"]')||node;
            var b=[...node.querySelectorAll('button,[role=button]')]
                .find(x=>/下载|保存|download/i.test(x.getAttribute('aria-label')||''));
            if(!b) return 'none';
            b.click();return 'ok';
        })(%s)`

	// engine.py:1027
	jsClearAttachmentsL1027 = "(function(){var btns=Array.from(document.querySelectorAll('button[aria-label]')).filter(function(b){var al=b.getAttribute('aria-label')||'';return /remove\\s*attachment|移除附件|删除附件|移除|删除/i.test(al);});btns.forEach(function(b){b.click();});Array.from(document.querySelectorAll('input[type=\"file\"]')).forEach(function(inp){inp.value='';});return btns.length;})()"

	// engine.py:1105
	jsAttachImageL1105 = "(function(){\n                var btns = Array.from(document.querySelectorAll('button[aria-label]'));\n                var hasRemove = btns.some(function(b){\n                    return /remove\\s*attachment|移除附件|删除附件/i.test(b.getAttribute('aria-label') || '');\n                });\n                if (hasRemove) return 'remove';\n                // 兜底：composer 内出现附件缩略图（img/video）\n                var ov = document.querySelector('[data-testid=\"hatch-composer-placeholder-overlay\"]');\n                var composer = ov ? (ov.closest('form') || ov.parentElement.parentElement.parentElement) : null;\n                if (composer) {\n                    var media = composer.querySelectorAll('img, video');\n                    if (media.length > 0) return 'thumb';\n                }\n                return '';\n                })()"

	// engine.py:272
	jsWaitWsReadyL272 = `(function(){
                    if (document.readyState !== 'complete') return 'loading';
                    if (!document.querySelector('textarea')) return 'no-ta';
                    var h = document.querySelector('[data-hatch-shell-hydration-state]');
                    if (h && h.getAttribute('data-hatch-shell-hydration-state') !== 'hydrated') return 'hydrating';
                    var b = document.body ? (document.body.innerText || '') : '';
                    if (b.indexOf('Connecting...') !== -1) return 'connecting';
                    return 'ready';
                })()`

	// engine.py:294
	jsResetThreadL294 = `(function(forChat){
                var d = document.querySelector('[role="dialog"]');
                if (d) {
                    var b = d.querySelector('button[aria-label*="close" i], button');
                    if (b) b.click();
                }
                var scope = document.querySelector('main,[class*="chat-scroll"],[class*="hatch-chat-scroll"]') || document.body;
                var bubbleCount = scope ? scope.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]').length : 0;
                var hasAtts = document.querySelectorAll('[data-testid^="hatch-chat-attachment-presentation-"]').length > 0;
                var hasStop = !!(document.querySelector('[data-testid="hatch-composer-stop-button"]')
                    || document.querySelector('button[aria-label*="Stop" i]')
                    || document.querySelector('button[aria-label*="停止"]'));
                var bodyTxt = document.body ? (document.body.innerText || '') : '';
                var hasStuck = bodyTxt.indexOf('Still sending') !== -1 || bodyTxt.indexOf('Connecting...') !== -1;
                if (hasStop || hasStuck || hasAtts) return true;
                if (forChat) {
                    return bubbleCount >= 24;
                }
                return (window.location.pathname !== '/thread/new') || bubbleCount > 0;
            })(%s)`

	// engine.py:458
	jsQuotaL458 = `(function(){var d=document.querySelector('[role=dialog],[aria-modal=true]');return d?(d.innerText||''):'';})()`

	// engine.py:735
	jsWaitAttachmentL735 = `(function(){
                    var bs=[].slice.call(document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]'))
                        .filter(function(b){return /hatch-agent-bubble-bg/.test(b.className||'');});
                    var lastTxt = bs.length ? (bs[bs.length-1].innerText||'').trim() : '';
                    var hasStop = !!(document.querySelector('[data-testid="hatch-composer-stop-button"]')
                        || document.querySelector('button[aria-label*="Stop" i]')
                        || document.querySelector('button[aria-label*="停止"]'));
                    var tail = document.body ? (document.body.innerText||'').slice(-700) : '';
                    return JSON.stringify({cnt: bs.length, txt: lastTxt, stop: hasStop, tail: tail});
                })()`

	// engine.py:319
	jsResetThreadL319 = `(function(){
                        return document.readyState === 'complete'
                            && !!document.querySelector('textarea')
                            && document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]').length === 0;
                    })()`

	// engine.py:631
	jsSendL631 = `!(document.querySelector('textarea')||{value:''}).value`

	// engine.py:386
	jsEnsurePageL386 = `document.body.innerText.slice(0,1200)`

	// engine.py:893
	jsChatStreamL893 = `document.body.innerText.slice(-500)`
)

// engine.py:1202 处由字面量与变量拼接而成的模板
func undefined() string {
	return `JSON.stringify({atts:[...document.querySelectorAll('` +
		attSel +
		`')].map(function(a){var m=a.querySelector('img,video');return {tid:a.getAttribute('data-testid'),src:m?(m.currentSrc||m.src||'').slice(0,60):''};}),buttons:[...document.querySelectorAll('button,[role=button]')].filter(b=>b.offsetParent!==null).map(b=>b.getAttribute('aria-label')||b.innerText.trim().slice(0,20)).filter(Boolean).slice(-40),tail:document.body.innerText.slice(-500)})`
}

// jsCatalog 让测试能按名字遍历所有独立常量。
var jsCatalog = map[string]string{
	"clickJS":                 clickJS,
	"attJS":                   attJS,
	"sendStateJS":             sendStateJS,
	"extractJS":               extractJS,
	"agentTextJS":             agentTextJS,
	"userCountJS":             userCountJS,
	"agentCountJS":            agentCountJS,
	"pollChatJS":              pollChatJS,
	"setterJS":                setterJS,
	"injectJS":                injectJS,
	"jsQuotaL438":             jsQuotaL438,
	"jsSendL571":              jsSendL571,
	"jsEnsurePageL377":        jsEnsurePageL377,
	"jsSendL559":              jsSendL559,
	"jsSendL605":              jsSendL605,
	"jsScrollBottomL649":      jsScrollBottomL649,
	"jsDownloadFallbackL965":  jsDownloadFallbackL965,
	"jsClearAttachmentsL1027": jsClearAttachmentsL1027,
	"jsAttachImageL1105":      jsAttachImageL1105,
	"jsWaitWsReadyL272":       jsWaitWsReadyL272,
	"jsResetThreadL294":       jsResetThreadL294,
	"jsQuotaL458":             jsQuotaL458,
	"jsWaitAttachmentL735":    jsWaitAttachmentL735,
	"jsResetThreadL319":       jsResetThreadL319,
	"jsSendL631":              jsSendL631,
	"jsEnsurePageL386":        jsEnsurePageL386,
	"jsChatStreamL893":        jsChatStreamL893,
}

// jsAssembled 是拼接模板的还原函数表，测试逐个与金样本比对。
var jsAssembled = map[string]func() string{
	"undefined": undefined,
}
