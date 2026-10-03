'use strict';
// Runs only inside an isolated diagnostics container. Credentials enter stdin.
const fs = require('node:fs');
const crypto = require('node:crypto');
const {chromium} = require('/app/webui/frontend/node_modules/playwright');
const sha = value => crypto.createHash('sha256').update(value).digest('hex');
(async () => {
  const input = JSON.parse(fs.readFileSync(0, 'utf8'));
  const saved = input.session;
  const data = saved.recovered;
  const secrets = [saved.email, saved.workspace_id, saved.expected_user_id, data.access_token, data.session_token, data.device_id].filter(Boolean);
  const redact = value => {
    let s = String(value);
    for (const secret of secrets) s = s.split(secret).join('[private]');
    return s.replace(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g, '[email]').replace(/eyJ[A-Za-z0-9_.-]{20,}/g, '[token]');
  };
  const emit = value => process.stdout.write(JSON.stringify(value) + '\n');
  let browser;
  try {
    if (saved.proxy !== 'socks5h://account-warp:40001') throw Error('Unexpected existing proxy');
    browser = await chromium.launch({headless:true, proxy:{server:'socks5://account-warp:40001'}, args:['--no-sandbox','--disable-dev-shm-usage']});
    const context = await browser.newContext({viewport:{width:1440,height:1000},locale:'en-US',timezoneId:'America/Los_Angeles'});
    const cookies = new Map();
    for (const part of (data.cookie_header || '').split(';')) {
      const at = part.indexOf('='); if (at < 1) continue;
      const name = part.slice(0,at).trim(), value = part.slice(at+1);
      if (name !== '__Secure-next-auth.session-token' && !name.startsWith('__Secure-next-auth.session-token.')) cookies.set(name,value);
    }
    const token = data.session_token;
    if (token.length > 3933) for(let at=0,index=0;at<token.length;at+=3933,index++) cookies.set('__Secure-next-auth.session-token.'+index,token.slice(at,at+3933));
    else cookies.set('__Secure-next-auth.session-token',token);
    cookies.set('oai-did',data.device_id);
    await context.addCookies([...cookies].map(([name,value])=>({name,value,domain:'chatgpt.com',path:'/',secure:true,sameSite:'Lax',httpOnly:name.includes('session-token')})));
    const page = await context.newPage();
    let generations=0;
    await page.route('**/backend-api/f/conversation', async route => {
      if(route.request().method()==='POST') {
        generations++;
        if(generations>1 || input.mode!=='generate') {emit({stage:'generation_blocked',attempt:generations});return route.abort();}
      }
      await route.continue();
    });
    page.on('response', response => {
      const url = new URL(response.url());
      if(url.pathname.startsWith('/backend-api/') && (/models|sentinel|conversation/.test(url.pathname))) emit({stage:'network',method:response.request().method(),path:url.pathname.replace(/\b[0-9a-f]{8}-[0-9a-f-]{27,}\b/gi,'[id]'),http:response.status()});
    });
    const response=await page.goto('https://chatgpt.com/',{waitUntil:'domcontentloaded',timeout:60000});
    await page.waitForTimeout(12000);
    emit({stage:'browser_page',browser_version:browser.version(),http:response?.status(),title:redact(await page.title()),body:redact((await page.locator('body').innerText()).slice(0,15000)),controls:await page.getByRole('button').evaluateAll(nodes=>nodes.map(n=>({text:n.innerText,aria:n.getAttribute('aria-label'),'data-testid':n.getAttribute('data-testid')}))).then(rows=>rows.map(r=>({text:redact(r.text),aria:redact(r.aria),'data-testid':r['data-testid']})))});
    if(input.mode==='inspect') {
      const modelButton=page.locator('[data-testid="model-switcher-dropdown-button"]');
      if(await modelButton.count()) {await modelButton.first().click();await page.waitForTimeout(1000);emit({stage:'model_menu',body:redact((await page.locator('body').innerText()).slice(-14000))});}
    }
    emit({stage:'complete',mode:input.mode,generation_requests:generations});
    await browser.close();
  } catch(error) {
    emit({stage:'browser_error',error_type:error.name,message:redact(error.message)});
    if(browser) await browser.close();process.exitCode=1;
  }
})();
