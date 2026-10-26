# ADR-008 — Aceleração por várias ligações (próximo passo para superar IDM e 1DM)

- **Estado:** implementado e testado localmente; o ganho em redes e servidores reais **ainda não foi medido** (ferramenta `svbench`).
- **Objetivo que decide:** superar o IDM e o 1DM. Por isso o critério de escolha não foi "o que é mais fácil", mas "que lacuna, se existir, faz um utilizador desses produtos recusar-nos".

## Como se decidiu
1. **Evidência no código:** `httpengine.Capabilities()` declarava `SupportsSegmentation: false`, ou seja, um download usava **uma só ligação**. Aceleração por várias ligações é a funcionalidade com que estes produtos se identificam.
2. **Alcance:** a segmentação melhora **todos** os downloads HTTP. HLS (a alternativa que tinha sugerido) só afeta os vídeos que usam streams. A segmentação vence por alcance e por ser a primeira comparação que qualquer utilizador faz (a velocidade).
3. **Onde já estamos à frente** (e não devia ser sacrificado): verificação de integridade antes de dar "concluído", retoma segura que deteta mudança no servidor, bloqueio de redirecionamentos para a rede interna, torrent/magnet integrado, envio entre dispositivos e deduplicação.

## O que foi feito
- N ligações com `Range` sobre segmentos pequenos servidos por uma fila (quem acaba primeiro puxa o seguinte). Ficheiro pré-alocado e escrito com `WriteAt`.
- **Retoma por segmento:** estado = bytes feitos de cada segmento, gravado **só depois de um fsync**. A divisão recalcula-se de `(Total, SegSize)`. Estados incoerentes recomeçam limpos.
- **Segurança dos dados:** `If-Range` em cada pedido (se o ficheiro mudou o servidor devolve 200 e o download aborta, nunca mistura versões); cada 206 é validado contra o intervalo e o total; servidor que ignora `Range` leva a fallback limpo para uma ligação.
- **Adaptação:** se o servidor limita ligações (429/503), os trabalhadores a mais reformam-se e a concorrência ajusta-se ao que o servidor aceita. Falhas de ligação repetem dentro do segmento, sem refazer o que já foi gravado.
- **Limitador de velocidade agregado** partilhado por todas as ligações.
- A 1.ª resposta (a sonda) é fechada logo: deixá-la aberta ocuparia um slot no servidor além das N ligações.
- Configuração: `network.connections_per_download` (defeito 8; 1 desliga; máx. 32) e `Policy.MaxConcurrency` por tarefa.

## Evidência medida (servidor simulado que limita cada ligação a ~1 MiB/s, ficheiro de 2 MiB)
| ligações | tempo | aceleração |
|---|---|---|
| 1 | 2,05 s | ×1,0 |
| 4 | 0,53 s | ×3,9 |
| 8 | 0,27 s | ×7,7 |

Duas mutações confirmam que os testes protegem o essencial: sem `If-Range` o teste de "ficheiro mudou a meio" falha; sem o limitador partilhado o teste de limite agregado falha.

## Limites (honestidade)
- **O ganho real depende do servidor.** Se o servidor limita por IP (e não por ligação), o ganho é pequeno. Muitos CDNs limitam por ligação, mas isto tem de ser medido: `svbench -url … -connections 1,2,4,8,16`.
- Não foi testado contra servidores reais. Não afirme números de velocidade (nem compare com o IDM) sem os medir na mesma rede e URL.
- Cookies: ver ADR-010 (suportados, só em memória e só para o host do link).

## Próximo passo recomendado
HLS **sem cifra** (VOD): cobre a maior parte dos vídeos embebidos que hoje a captura lista mas não consegue descarregar. **Feito: ver ADR-009.** Depois, cookies: **feito, ver ADR-010** (credenciais só em memória, em vez de armazenamento persistente).
