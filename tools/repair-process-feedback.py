from pathlib import Path

path = Path('internal/webui/access.html')
source = path.read_text(encoding='utf-8')

def replace_once(old, new):
    global source
    count = source.count(old)
    if count != 1:
        raise SystemExit(f'Expected one patch anchor, got {count}: {old[:100]!r}')
    source = source.replace(old, new, 1)

replace_once(" '실행 확인 필요':['Check run status','起動状態を確認'],\n '중지 확인 필요':['Check stop status','停止状態を確認'],\n '재시작 확인 필요':['Check restart status','再起動状態を確認'],", " '상태 확인 중':['Checking status…','状態確認中'],")
replace_once(""" const suffix=feedback.status==='pending'?' 요청 중':feedback.status==='unknown'?' 확인 필요':' 실패';
 return '<span class="process-action-feedback">'+badge(feedback.label+suffix,feedback.status==='pending'?'blue':'amber')+'</span>';
}
function setLogDockRatio(""", """ const text=feedback.status==='unknown'?'상태 확인 중':feedback.label+(feedback.status==='pending'?' 요청 중':' 실패');
 return '<span class="process-action-feedback">'+badge(text,feedback.status==='error'?'amber':'blue')+'</span>';
}
function reconcileProcessFeedback(snapshot){
 // A fresh state read resolves uncertainty, not the outcome of a lost command.
 // Keep confirmed errors and in-flight actions. An older read must not clear
 // feedback created or replaced after that read started.
 for(const [id,feedback] of snapshot){
  if(feedback.status==='unknown'&&processFeedback.get(id)===feedback)processFeedback.delete(id);
 }
}
function setLogDockRatio(""")
replace_once("""async function loadState(){
 const data=await request('/api/access/state');""", """async function loadState(){
 const feedbackSnapshot=new Map(processFeedback);
 const data=await request('/api/access/state');""")
replace_once(""" if(!db.processGroups.some(g=>g.id===state.pgFilter))state.pgFilter='all';
}
async function refresh(){""", """ if(!db.processGroups.some(g=>g.id===state.pgFilter))state.pgFilter='all';
 reconcileProcessFeedback(feedbackSnapshot);
}
async function refresh(){""")
replace_once("""  try{
   const result=await request('/processes/'+encodeURIComponent(id)+'/'+action,{});
   await refresh();
   if(result.process?.status==='failed')throw new Error(result.process.error||'프로세스를 실행하지 못했습니다.');
   processFeedback.delete(id);
   render();
   void pollLog(true);
   toast(label+' 요청을 처리했습니다.');
  }catch(error){
   processFeedback.set(id,{label,status:error.network?'unknown':'error'});
   render();
   handleError(error);
  }
""", """  try{
   const result=await request('/processes/'+encodeURIComponent(id)+'/'+action,{});
   if(result.process?.status==='failed')throw new Error(result.process.error||'프로세스를 실행하지 못했습니다.');
  }catch(error){
   processFeedback.set(id,{label,status:error.network?'unknown':'error'});
   render();
   handleError(error);
   return;
  }
  try{
   await refresh();
   processFeedback.delete(id);
   render();
   void pollLog(true);
   toast(label+' 요청을 처리했습니다.');
  }catch(error){
   // The command was acknowledged. A failed read must not turn that into a
   // command failure or cause a replay; background state reads reconcile it.
   processFeedback.set(id,{label,status:'unknown'});
   render();
   handleError(error);
  }
""")
path.write_text(source, encoding='utf-8')
print('Applied process feedback reconciliation patch')
