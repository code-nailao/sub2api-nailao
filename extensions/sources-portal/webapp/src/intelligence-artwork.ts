export interface ArtworkSize { width: number; height: number }
export const initialArtworkSize: ArtworkSize = { width: 1280, height: 960 }

/** Untrusted model HTML runs only in a network-blocked, opaque-origin iframe. */
export function artworkDocument(html: string, channel: string): string {
  if (!html.trim() || html.length > 2 * 1024 * 1024) throw new Error('作品内容不可用')
  const policy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'"
  const body = html.replace(/^\s*<!doctype[^>]*>/i, '')
  const bridge = '(function(){const channel=' + JSON.stringify(channel) +
    ',target=' + JSON.stringify(location.origin) + ';let count=0,last="";' +
    'function measure(){if(count>=8)return;const d=document.documentElement,b=document.body;' +
    'const width=Math.ceil(Math.max(1280,d.scrollWidth,b?b.scrollWidth:0));' +
    'const height=Math.ceil(Math.max(960,d.scrollHeight,b?b.scrollHeight:0));' +
    'const key=width+":"+height;if(last===key)return;last=key;count++;' +
    'parent.postMessage({type:"sources:intelligence-artwork-size",channel,width,height},target)}' +
    'addEventListener("load",()=>{measure();new ResizeObserver(measure).observe(document.documentElement)});' +
    'requestAnimationFrame(measure);setTimeout(measure,200);setTimeout(measure,1000)})();'
  return '<!doctype html><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="' +
    policy + '"><meta name="referrer" content="no-referrer">' + body + '<script>' + bridge + '</script>'
}

export function validArtworkSize(value: unknown): value is ArtworkSize {
  if (!value || typeof value !== 'object') return false
  const size = value as ArtworkSize
  return Number.isFinite(size.width) && Number.isFinite(size.height) &&
    size.width >= 256 && size.width <= 4096 && size.height >= 256 && size.height <= 8192
}
