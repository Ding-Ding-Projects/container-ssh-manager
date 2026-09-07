export const esc=(value:string)=>value.replace(/[&<>'"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[c]!));
export const query=(path:string, values:Record<string,string|number|boolean|undefined>)=>path+'?'+Object.entries(values).filter(([,v])=>v!==undefined).map(([k,v])=>`${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`).join('&');
export const cronValid=(cron:string)=>cron.trim().split(/\s+/).length===5;
export const positive=(value:string)=>Number.isInteger(Number(value))&&Number(value)>0;
