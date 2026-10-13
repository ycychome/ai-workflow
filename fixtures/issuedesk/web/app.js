'use strict';
const $ = id => document.getElementById(id);
let token = '', actor = null, selected = null, cursor = '', createKey = null;
const names = {open:'待处理',in_progress:'处理中',resolved:'已解决',closed:'已关闭',cancelled:'已取消'};
function notice(text) { $('notice').textContent = text; }
async function api(path, options = {}) {
 const response = await fetch(path, {...options, headers: {'Authorization':`Bearer ${token}`,'Content-Type':'application/json',...(options.headers || {})}});
 const body = response.status === 204 ? null : await response.json();
 if (!response.ok) throw new Error(body.error?.message || '请求失败');
 return body;
}
function wrap(fn) { return async event => { event.preventDefault(); try { await fn(); } catch (error) { notice(error.message); } }; }
async function list(next = false) {
 const q = new URLSearchParams({q:$('query').value,status:$('status').value,limit:'10'});
 if (next && cursor) q.set('cursor',cursor);
 const page = await api(`/api/tickets?${q}`); if (!next) $('tickets').replaceChildren();
 for (const ticket of page.items) { const button = document.createElement('button'); button.textContent = `#${ticket.id} ${ticket.title} · ${names[ticket.status]}`; button.addEventListener('click', wrap(() => detail(ticket.id))); $('tickets').append(button); }
 cursor = page.next_cursor || ''; $('more').hidden = !cursor;
}
async function detail(id) {
 selected = await api(`/api/tickets/${id}`); $('detail').hidden = false;
 $('ticket-title').textContent = selected.title; $('ticket-body').textContent = selected.body; $('ticket-meta').textContent = `版本 ${selected.version} · ${names[selected.status]}`;
 $('edit-title').value = selected.title; $('edit-body').value = selected.body; $('edit-status').value = selected.status; $('assignee').value = selected.assignee_id || ''; $('assignee').disabled = actor.role === 'requester'; $('delete').hidden = actor.role !== 'admin';
 const page = await api(`/api/tickets/${id}/comments`); $('comments').replaceChildren();
 for (const comment of page.items) { const p = document.createElement('p'); p.textContent = `用户 ${comment.author_id}：${comment.body}`; $('comments').append(p); }
}
$('login').addEventListener('submit',wrap(async () => { token = $('token').value; actor = await api('/api/me'); $('token').value = ''; $('identity').textContent = `${actor.name} · ${actor.role} · 租户 ${actor.tenant_id}`; $('workspace').hidden = false; $('detail').hidden = true; selected = null; createKey = null; await list(); notice('已进入工作台'); }));
$('create').addEventListener('input',() => { createKey = null; });
$('create').addEventListener('submit',wrap(async () => { createKey ||= crypto.randomUUID(); const ticket = await api('/api/tickets',{method:'POST',headers:{'Idempotency-Key':createKey},body:JSON.stringify({title:$('title').value,body:$('body').value})}); createKey = null; $('create').reset(); await list(); await detail(ticket.id); notice('工单已创建'); }));
$('filter').addEventListener('submit',wrap(() => list())); $('more').addEventListener('click',wrap(() => list(true)));
$('edit').addEventListener('submit',wrap(async () => { const body = {title:$('edit-title').value,body:$('edit-body').value,status:$('edit-status').value}; if(actor.role!=='requester'){if($('assignee').value) body.assignee_id=Number($('assignee').value);else body.unassign=true;} await api(`/api/tickets/${selected.id}`,{method:'PATCH',headers:{'If-Match':`"${selected.version}"`},body:JSON.stringify(body)}); await detail(selected.id); await list(); notice('工单已保存'); }));
$('comment').addEventListener('submit',wrap(async () => { await api(`/api/tickets/${selected.id}/comments`,{method:'POST',headers:{'If-Match':`"${selected.version}"`},body:JSON.stringify({body:$('comment-body').value})}); $('comment').reset(); await detail(selected.id); notice('评论已发送'); }));
$('delete').addEventListener('click',wrap(async () => { if(!confirm('确认删除此工单？')) return; await api(`/api/tickets/${selected.id}`,{method:'DELETE',headers:{'If-Match':`"${selected.version}"`}}); selected=null; $('detail').hidden=true; await list(); notice('工单已删除'); }));
